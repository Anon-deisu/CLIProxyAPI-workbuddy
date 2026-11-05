package responses

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/ojson"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

type oaiToResponsesStateReasoning struct {
	ReasoningID   string
	ReasoningData string
	OutputIndex   int
}
type oaiToResponsesState struct {
	translatorcommon.ApplyPatchErrorState
	ApplyPatchCalls map[string]*translatorcommon.ApplyPatchCallState
	// Request fields and tool identities are captured once for this stream.
	RequestEcho        responsesRequestEcho
	RequestInitialized bool
	Seq                int
	ResponseID         string
	Created            int64
	Started            bool
	CompletedEmitted   bool
	CompletionPending  bool
	ReasoningID        string
	ReasoningIndex     int
	// aggregation buffers for response.output
	// Per-output message text buffers by index
	MsgTextBuf            map[int]*strings.Builder
	ReasoningBuf          strings.Builder
	Reasonings            []oaiToResponsesStateReasoning
	FuncArgsBuf           map[string]*strings.Builder
	FuncNames             map[string]string
	FuncCallIDs           map[string]string
	FuncIdentityConflicts map[string]bool
	FuncOutputIx          map[string]int
	FuncArgsSent          map[string]int
	MsgOutputIx           map[int]int
	NextOutputIx          int
	// message item state per output index
	MsgItemAdded    map[int]bool // whether response.output_item.added emitted for message
	MsgContentAdded map[int]bool // whether response.content_part.added emitted for message
	MsgItemDone     map[int]bool // whether message done events were emitted
	// function item state
	FuncItemAdded  map[string]bool
	FuncItemCustom map[string]bool
	FuncArgsDone   map[string]bool
	FuncItemDone   map[string]bool
	// names of freeform ("custom") tools from the original request; calls to
	// these are emitted as custom_tool_call items instead of function_call
	CustomToolNames map[string]struct{}
	ToolIndex       *responsesToolIdentityIndex
	FinishReason    string
	// usage aggregation
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	ReasoningTokens  int64
	UsageSeen        bool
}

// responseIDCounter provides a process-wide unique counter for synthesized response identifiers.
var responseIDCounter uint64

func emitRespEvent(event string, payload []byte) []byte {
	return translatorcommon.SSEEventData(event, payload)
}

func incompleteByFinishReason(reason string) ([]byte, bool) {
	switch reason {
	case "length", "max_tokens":
		return []byte(`{"reason":"max_output_tokens"}`), true
	case "content_filter":
		return []byte(`{"reason":"content_filter"}`), true
	default:
		return nil, false
	}
}

func buildResponsesCompletedEvent(st *oaiToResponsesState, nextSeq func() int) []byte {
	eventType := "response.completed"
	status := "completed"
	incompleteDetails, isIncomplete := incompleteByFinishReason(st.FinishReason)
	if isIncomplete {
		eventType = "response.incomplete"
		status = "incomplete"
	}

	completedDoc := ojson.NewDocString(`{"type":"","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"","background":false,"error":null}}`)
	_ = completedDoc.Set("type", eventType)
	_ = completedDoc.Set("sequence_number", nextSeq())
	_ = completedDoc.Set("response.id", st.ResponseID)
	_ = completedDoc.Set("response.created_at", st.Created)
	_ = completedDoc.Set("response.status", status)
	if len(incompleteDetails) > 0 {
		_ = completedDoc.SetRawBytes("response.incomplete_details", incompleteDetails)
	}
	// Inject original request fields into response as per docs/response.completed.json
	st.RequestEcho.applyDoc(completedDoc, "response.")

	type completedOutputItem struct {
		index int
		raw   []byte
	}
	outputItems := make([]completedOutputItem, 0, len(st.Reasonings)+len(st.MsgItemAdded)+len(st.FuncArgsBuf))
	if len(st.Reasonings) > 0 {
		for _, r := range st.Reasonings {
			itemDoc := ojson.NewDocString(`{"id":"","type":"reasoning","summary":[{"type":"summary_text","text":""}]}`)
			_ = itemDoc.Set("id", r.ReasoningID)
			_ = itemDoc.Set("summary.0.text", r.ReasoningData)
			outputItems = append(outputItems, completedOutputItem{index: r.OutputIndex, raw: itemDoc.Bytes()})
		}
	}
	if len(st.MsgItemAdded) > 0 {
		for i := range st.MsgItemAdded {
			txt := ""
			if b := st.MsgTextBuf[i]; b != nil {
				txt = b.String()
			}
			msgStatus := "completed"
			if _, isInc := incompleteByFinishReason(st.FinishReason); isInc {
				msgStatus = "incomplete"
			}
			itemDoc := ojson.NewDocString(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
			_ = itemDoc.Set("id", fmt.Sprintf("msg_%s_%d", st.ResponseID, i))
			_ = itemDoc.Set("status", msgStatus)
			_ = itemDoc.Set("content.0.text", txt)
			outputItems = append(outputItems, completedOutputItem{index: st.MsgOutputIx[i], raw: itemDoc.Bytes()})
		}
	}
	if len(st.FuncArgsBuf) > 0 {
		for key := range st.FuncArgsBuf {
			if !st.FuncItemDone[key] {
				continue
			}
			args := ""
			if b := st.FuncArgsBuf[key]; b != nil {
				args = b.String()
			}
			callID := st.FuncCallIDs[key]
			name := st.FuncNames[key]
			toolStatus := "completed"
			if _, isInc := incompleteByFinishReason(st.FinishReason); isInc {
				toolStatus = "incomplete"
			}
			if st.ToolIndex.isShell(name) {
				// The action was validated before the item was marked done.
				item, _ := shellCallItem(callID, args, toolStatus)
				outputItems = append(outputItems, completedOutputItem{index: st.FuncOutputIx[key], raw: item})
				continue
			}
			if st.FuncItemCustom[key] {
				itemDoc := ojson.NewDocString(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
				_ = itemDoc.Set("id", fmt.Sprintf("ctc_%s", callID))
				_ = itemDoc.Set("status", toolStatus)
				input := unwrapCustomToolInput(args)
				if patchCall := st.ApplyPatchCalls[key]; patchCall != nil {
					input = patchCall.Decoder.Input()
				}
				_ = itemDoc.Set("input", input)
				_ = itemDoc.Set("call_id", callID)
				st.ToolIndex.applyIdentityDoc(itemDoc, name, "")
				outputItems = append(outputItems, completedOutputItem{index: st.FuncOutputIx[key], raw: itemDoc.Bytes()})
				continue
			}
			itemDoc := ojson.NewDocString(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
			_ = itemDoc.Set("id", fmt.Sprintf("fc_%s", callID))
			_ = itemDoc.Set("status", toolStatus)
			_ = translatorcommon.SetStringWithoutHTMLEscapeDoc(itemDoc, "arguments", args)
			_ = itemDoc.Set("call_id", callID)
			st.ToolIndex.applyIdentityDoc(itemDoc, name, "")
			outputItems = append(outputItems, completedOutputItem{index: st.FuncOutputIx[key], raw: itemDoc.Bytes()})
		}
	}
	sort.Slice(outputItems, func(i, j int) bool { return outputItems[i].index < outputItems[j].index })
	outputs := make([][]byte, 0, len(outputItems))
	for _, item := range outputItems {
		outputs = append(outputs, item.raw)
	}
	if len(outputs) > 0 {
		_ = completedDoc.SetRawBytes("response.output", translatorcommon.JoinRawArray(outputs))
	}
	if st.UsageSeen {
		_ = completedDoc.Set("response.usage.input_tokens", st.PromptTokens)
		_ = completedDoc.Set("response.usage.input_tokens_details.cached_tokens", st.CachedTokens)
		_ = completedDoc.Set("response.usage.output_tokens", st.CompletionTokens)
		if st.ReasoningTokens > 0 {
			_ = completedDoc.Set("response.usage.output_tokens_details.reasoning_tokens", st.ReasoningTokens)
		}
		total := st.TotalTokens
		if total == 0 {
			total = st.PromptTokens + st.CompletionTokens
		}
		_ = completedDoc.Set("response.usage.total_tokens", total)
	}
	return emitRespEvent(eventType, completedDoc.Bytes())
}

// ConvertOpenAIChatCompletionsResponseToOpenAIResponses converts OpenAI Chat Completions streaming chunks
// to OpenAI Responses SSE events (response.*).
func ConvertOpenAIChatCompletionsResponseToOpenAIResponses(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &oaiToResponsesState{}
	}
	st := (*param).(*oaiToResponsesState)
	if st.ToolInputError() != nil || st.CompletedEmitted {
		return nil
	}

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	rawJSON = bytes.TrimSpace(rawJSON)
	if !st.RequestInitialized {
		requestRoot := util.ParseGJSONBytesNoCopy(bytes.Clone(pickRequestJSON(originalRequestRawJSON, requestRawJSON)))
		st.ToolIndex = newResponsesToolIndex(requestRoot)
		st.RequestEcho = extractResponsesRequestEcho(requestRoot)
		st.RequestInitialized = true
	}
	if len(rawJSON) == 0 {
		return nil
	}
	isDone := bytes.Equal(rawJSON, []byte("[DONE]"))
	if isDone && (!st.Started || st.CompletedEmitted) {
		return [][]byte{}
	}

	root := util.ParseGJSONBytesNoCopy(rawJSON)
	if !isDone {
		obj := root.Get("object")
		if obj.Exists() && obj.String() != "" && obj.String() != "chat.completion.chunk" {
			return [][]byte{}
		}
		if !root.Get("choices").Exists() || !root.Get("choices").IsArray() {
			return [][]byte{}
		}
	}

	if usage := root.Get("usage"); usage.Exists() {
		if v := usage.Get("prompt_tokens"); v.Exists() {
			st.PromptTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			st.CachedTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("completion_tokens"); v.Exists() {
			st.CompletionTokens = v.Int()
			st.UsageSeen = true
		} else if v := usage.Get("output_tokens"); v.Exists() {
			st.CompletionTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
			st.ReasoningTokens = v.Int()
			st.UsageSeen = true
		} else if v := usage.Get("completion_tokens_details.reasoning_tokens"); v.Exists() {
			st.ReasoningTokens = v.Int()
			st.UsageSeen = true
		}
		if v := usage.Get("total_tokens"); v.Exists() {
			st.TotalTokens = v.Int()
			st.UsageSeen = true
		}
	}

	nextSeq := func() int { st.Seq++; return st.Seq }
	allocOutputIndex := func() int {
		ix := st.NextOutputIx
		st.NextOutputIx++
		return ix
	}
	toolStateKey := func(outputIndex, toolIndex int) string { return fmt.Sprintf("%d:%d", outputIndex, toolIndex) }
	var out [][]byte
	failToolInput := func(err error) {
		if st.ToolInputError() == nil {
			st.SetToolInputError(err)
			out = append(out, emitRespEvent("response.failed", responsesToolInputFailure(st.ResponseID, nextSeq(), err)))
		}
	}
	emitToolItem := func(key string, force bool) {
		if st.FuncItemAdded[key] {
			return
		}
		callID := st.FuncCallIDs[key]
		name := st.ToolIndex.canonical(st.FuncNames[key])
		st.FuncNames[key] = name
		if !force && (callID == "" || name == "") {
			return
		}
		if name == "" {
			if customToolName, ok := st.ToolIndex.singleCustomToolName(); ok {
				name = customToolName
				st.FuncNames[key] = customToolName
			}
		}
		if st.ToolIndex.isApplyPatch(name) && st.FuncIdentityConflicts[key] {
			failToolInput(fmt.Errorf("conflicting apply_patch call identity"))
			return
		}
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%s", st.ResponseID, strings.ReplaceAll(key, ":", "_"))
			st.FuncCallIDs[key] = callID
		}

		outputIndex := st.FuncOutputIx[key]
		_, isCustomTool := st.CustomToolNames[name]
		st.FuncItemCustom[key] = isCustomTool
		if st.ToolIndex.isShell(name) {
			oDoc := ojson.NewDocString(`{"type":"response.output_item.added"}`)
			_ = oDoc.Set("sequence_number", nextSeq())
			_ = oDoc.Set("output_index", outputIndex)
			_ = oDoc.SetRawBytes("item", shellCallPlaceholder(callID))
			out = append(out, emitRespEvent("response.output_item.added", oDoc.Bytes()))
		} else if isCustomTool {
			if st.ToolIndex.isApplyPatch(name) {
				d := st.ToolIndex.byChat[name]
				st.ApplyPatchCalls[key] = &translatorcommon.ApplyPatchCallState{
					ItemID: fmt.Sprintf("ctc_%s", callID), CallID: callID,
					Name: d.localName, Namespace: d.namespace, OutputIndex: outputIndex,
				}
			}
			oDoc := ojson.NewDocString(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"in_progress","input":"","call_id":"","name":""}}`)
			_ = oDoc.Set("sequence_number", nextSeq())
			_ = oDoc.Set("output_index", outputIndex)
			_ = oDoc.Set("item.id", fmt.Sprintf("ctc_%s", callID))
			_ = oDoc.Set("item.call_id", callID)
			st.ToolIndex.applyIdentityDoc(oDoc, name, "item")
			out = append(out, emitRespEvent("response.output_item.added", oDoc.Bytes()))
		} else {
			oDoc := ojson.NewDocString(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"in_progress","arguments":"","call_id":"","name":""}}`)
			_ = oDoc.Set("sequence_number", nextSeq())
			_ = oDoc.Set("output_index", outputIndex)
			_ = oDoc.Set("item.id", fmt.Sprintf("fc_%s", callID))
			_ = oDoc.Set("item.call_id", callID)
			st.ToolIndex.applyIdentityDoc(oDoc, name, "item")
			out = append(out, emitRespEvent("response.output_item.added", oDoc.Bytes()))
		}
		st.FuncItemAdded[key] = true
	}
	emitPendingFunctionArgs := func(key string) {
		if !st.FuncItemAdded[key] || st.ToolInputError() != nil || st.ToolIndex.isShell(st.FuncNames[key]) {
			return
		}
		argsBuf := st.FuncArgsBuf[key]
		if argsBuf == nil || argsBuf.Len() <= st.FuncArgsSent[key] {
			return
		}
		args := argsBuf.String()
		delta := args[st.FuncArgsSent[key]:]
		if st.FuncItemCustom[key] {
			if patchCall := st.ApplyPatchCalls[key]; patchCall != nil {
				patchDelta, errPushArguments := patchCall.PushArguments(delta)
				if errPushArguments != nil {
					failToolInput(errPushArguments)
				} else if patchDelta != "" {
					out = append(out, emitRespEvent("response.custom_tool_call_input.delta", translatorcommon.ApplyPatchInputDelta(patchCall, patchDelta, nextSeq())))
				}
				st.FuncArgsSent[key] = len(args)
			}
			return
		}
		callID := st.FuncCallIDs[key]
		adDoc := ojson.NewDocString(`{"type":"response.function_call_arguments.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`)
		_ = adDoc.Set("sequence_number", nextSeq())
		_ = adDoc.Set("item_id", fmt.Sprintf("fc_%s", callID))
		_ = adDoc.Set("output_index", st.FuncOutputIx[key])
		_ = translatorcommon.SetStringWithoutHTMLEscapeDoc(adDoc, "delta", delta)
		out = append(out, emitRespEvent("response.function_call_arguments.delta", adDoc.Bytes()))
		st.FuncArgsSent[key] = len(args)
	}

	if !st.Started {
		st.ResponseID = strings.Clone(root.Get("id").String())
		st.Created = root.Get("created").Int()
		// reset aggregation state for a new streaming response
		st.MsgTextBuf = make(map[int]*strings.Builder)
		st.ReasoningBuf.Reset()
		st.ReasoningID = ""
		st.ReasoningIndex = 0
		st.ApplyPatchCalls = make(map[string]*translatorcommon.ApplyPatchCallState)
		st.FuncArgsBuf = make(map[string]*strings.Builder)
		st.FuncNames = make(map[string]string)
		st.FuncCallIDs = make(map[string]string)
		st.FuncIdentityConflicts = make(map[string]bool)
		st.FuncOutputIx = make(map[string]int)
		st.FuncArgsSent = make(map[string]int)
		st.MsgOutputIx = make(map[int]int)
		st.NextOutputIx = 0
		st.MsgItemAdded = make(map[int]bool)
		st.MsgContentAdded = make(map[int]bool)
		st.MsgItemDone = make(map[int]bool)
		st.FuncItemAdded = make(map[string]bool)
		st.FuncItemCustom = make(map[string]bool)
		st.FuncArgsDone = make(map[string]bool)
		st.FuncItemDone = make(map[string]bool)
		st.CustomToolNames = st.ToolIndex.custom
		st.PromptTokens = 0
		st.CachedTokens = 0
		st.CompletionTokens = 0
		st.TotalTokens = 0
		st.ReasoningTokens = 0
		st.FinishReason = ""
		st.UsageSeen = false
		st.CompletedEmitted = false
		st.CompletionPending = false
		// response.created
		createdDoc := ojson.NewDocString(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
		_ = createdDoc.Set("sequence_number", nextSeq())
		_ = createdDoc.Set("response.id", st.ResponseID)
		_ = createdDoc.Set("response.created_at", st.Created)
		requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
		if requestModelName == "" {
			requestModelName = modelName
		}
		if requestModelName != "" {
			_ = createdDoc.Set("response.model", requestModelName)
		}
		out = append(out, emitRespEvent("response.created", createdDoc.Bytes()))

		inprogDoc := ojson.NewDocString(`{"type":"response.in_progress","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","output":[]}}`)
		_ = inprogDoc.Set("sequence_number", nextSeq())
		_ = inprogDoc.Set("response.id", st.ResponseID)
		_ = inprogDoc.Set("response.created_at", st.Created)
		if requestModelName != "" {
			_ = inprogDoc.Set("response.model", requestModelName)
		}
		out = append(out, emitRespEvent("response.in_progress", inprogDoc.Bytes()))
		st.Started = true
	}

	stopReasoning := func(text string) {
		// Emit reasoning done events
		textDoneDoc := ojson.NewDocString(`{"type":"response.reasoning_summary_text.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`)
		_ = textDoneDoc.Set("sequence_number", nextSeq())
		_ = textDoneDoc.Set("item_id", st.ReasoningID)
		_ = textDoneDoc.Set("output_index", st.ReasoningIndex)
		_ = textDoneDoc.Set("text", text)
		out = append(out, emitRespEvent("response.reasoning_summary_text.done", textDoneDoc.Bytes()))
		partDoneDoc := ojson.NewDocString(`{"type":"response.reasoning_summary_part.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
		_ = partDoneDoc.Set("sequence_number", nextSeq())
		_ = partDoneDoc.Set("item_id", st.ReasoningID)
		_ = partDoneDoc.Set("output_index", st.ReasoningIndex)
		_ = partDoneDoc.Set("part.text", text)
		out = append(out, emitRespEvent("response.reasoning_summary_part.done", partDoneDoc.Bytes()))
		outputItemDoneDoc := ojson.NewDocString(`{"type":"response.output_item.done","item":{"id":"","type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":""}]},"output_index":0,"sequence_number":0}`)
		_ = outputItemDoneDoc.Set("sequence_number", nextSeq())
		_ = outputItemDoneDoc.Set("item.id", st.ReasoningID)
		_ = outputItemDoneDoc.Set("output_index", st.ReasoningIndex)
		_ = outputItemDoneDoc.Set("item.summary.0.text", text)
		out = append(out, emitRespEvent("response.output_item.done", outputItemDoneDoc.Bytes()))

		st.Reasonings = append(st.Reasonings, oaiToResponsesStateReasoning{ReasoningID: st.ReasoningID, ReasoningData: text, OutputIndex: st.ReasoningIndex})
		st.ReasoningID = ""
	}

	emitMessageItemDone := func(idx int) {
		if !st.MsgItemAdded[idx] || st.MsgItemDone[idx] {
			return
		}
		msgOutputIndex := st.MsgOutputIx[idx]
		fullText := ""
		if b := st.MsgTextBuf[idx]; b != nil {
			fullText = b.String()
		}
		doneDoc := ojson.NewDocString(`{"type":"response.output_text.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"text":"","logprobs":[]}`)
		_ = doneDoc.Set("sequence_number", nextSeq())
		_ = doneDoc.Set("item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
		_ = doneDoc.Set("output_index", msgOutputIndex)
		_ = doneDoc.Set("content_index", 0)
		_ = doneDoc.Set("text", fullText)
		out = append(out, emitRespEvent("response.output_text.done", doneDoc.Bytes()))

		partDoneDoc := ojson.NewDocString(`{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
		_ = partDoneDoc.Set("sequence_number", nextSeq())
		_ = partDoneDoc.Set("item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
		_ = partDoneDoc.Set("output_index", msgOutputIndex)
		_ = partDoneDoc.Set("content_index", 0)
		_ = partDoneDoc.Set("part.text", fullText)
		out = append(out, emitRespEvent("response.content_part.done", partDoneDoc.Bytes()))

		msgStatus := "completed"
		if _, isInc := incompleteByFinishReason(st.FinishReason); isInc {
			msgStatus = "incomplete"
		}
		itemDoneDoc := ojson.NewDocString(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}}`)
		_ = itemDoneDoc.Set("sequence_number", nextSeq())
		_ = itemDoneDoc.Set("output_index", msgOutputIndex)
		_ = itemDoneDoc.Set("item.id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
		_ = itemDoneDoc.Set("item.status", msgStatus)
		_ = itemDoneDoc.Set("item.content.0.text", fullText)
		out = append(out, emitRespEvent("response.output_item.done", itemDoneDoc.Bytes()))
		st.MsgItemDone[idx] = true
	}

	finalizeOpenItems := func() {
		if st.ToolInputError() != nil {
			return
		}
		if len(st.MsgItemAdded) > 0 {
			idxs := make([]int, 0, len(st.MsgItemAdded))
			for idx := range st.MsgItemAdded {
				idxs = append(idxs, idx)
			}
			sort.Slice(idxs, func(i, j int) bool { return st.MsgOutputIx[idxs[i]] < st.MsgOutputIx[idxs[j]] })
			for _, idx := range idxs {
				emitMessageItemDone(idx)
			}
		}

		if st.ReasoningID != "" {
			stopReasoning(st.ReasoningBuf.String())
			st.ReasoningBuf.Reset()
		}

		if len(st.FuncArgsBuf) == 0 {
			return
		}
		keys := make([]string, 0, len(st.FuncArgsBuf))
		for key := range st.FuncArgsBuf {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			left := st.FuncOutputIx[keys[i]]
			right := st.FuncOutputIx[keys[j]]
			return left < right || (left == right && keys[i] < keys[j])
		})
		for _, key := range keys {
			if st.FuncItemDone[key] {
				continue
			}
			b := st.FuncArgsBuf[key]
			hasArgs := b != nil && b.Len() > 0
			_, isIncomplete := incompleteByFinishReason(st.FinishReason)
			isExplicitToolFinish := st.FinishReason == "tool_calls" || st.FinishReason == "stop"

			// If stream ended without finish_reason:
			// If no arguments or partial/invalid JSON arguments were received, do not synthesize empty arguments
			// or complete the in-flight tool call item as successfully completed.
			name := st.ToolIndex.canonicalName(st.FuncNames[key])
			if name == "" {
				name, _ = st.ToolIndex.singleCustomName()
			}
			if !st.ToolIndex.isApplyPatch(name) && st.FinishReason == "" && (!hasArgs || !ojson.Valid(b.String())) {
				continue
			}

			emitToolItem(key, true)
			emitPendingFunctionArgs(key)
			if st.ToolInputError() != nil {
				return
			}
			callID := st.FuncCallIDs[key]
			if callID == "" || st.FuncItemDone[key] {
				continue
			}

			outputIndex := st.FuncOutputIx[key]
			toolStatus := "completed"
			args := "{}"
			if hasArgs {
				args = b.String()
			} else if isIncomplete || !isExplicitToolFinish {
				args = ""
			}
			if isIncomplete {
				toolStatus = "incomplete"
			}

			if st.ToolIndex.isShell(name) {
				item, errShellCallItem := shellCallItem(callID, args, toolStatus)
				if errShellCallItem != nil {
					failToolInput(errShellCallItem)
					return
				}
				eventDoc := ojson.NewDocString(`{"type":"response.output_item.done"}`)
				_ = eventDoc.Set("sequence_number", nextSeq())
				_ = eventDoc.Set("output_index", outputIndex)
				_ = eventDoc.SetRawBytes("item", item)
				out = append(out, emitRespEvent("response.output_item.done", eventDoc.Bytes()))
				st.FuncItemDone[key] = true
				st.FuncArgsDone[key] = true
				continue
			}

			if st.FuncItemCustom[key] {
				input := ""
				if patchCall := st.ApplyPatchCalls[key]; patchCall != nil {
					tail, fullInput, errFinishArguments := patchCall.FinishArguments(args)
					if errFinishArguments != nil {
						failToolInput(errFinishArguments)
						return
					}
					input = fullInput
					if tail != "" {
						out = append(out, emitRespEvent("response.custom_tool_call_input.delta", translatorcommon.ApplyPatchInputDelta(patchCall, tail, nextSeq())))
					}
					out = append(out, emitRespEvent("response.custom_tool_call_input.done", translatorcommon.ApplyPatchInputDone(patchCall, input, nextSeq())))
				} else {
					input = unwrapCustomToolInput(args)
					inputDoneDoc := ojson.NewDocString(`{"type":"response.custom_tool_call_input.done","sequence_number":0,"item_id":"","output_index":0,"input":""}`)
					_ = inputDoneDoc.Set("sequence_number", nextSeq())
					_ = inputDoneDoc.Set("item_id", fmt.Sprintf("ctc_%s", callID))
					_ = inputDoneDoc.Set("output_index", outputIndex)
					_ = inputDoneDoc.Set("input", input)
					out = append(out, emitRespEvent("response.custom_tool_call_input.done", inputDoneDoc.Bytes()))
				}

				itemDoneDoc := ojson.NewDocString(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}}`)
				_ = itemDoneDoc.Set("sequence_number", nextSeq())
				_ = itemDoneDoc.Set("output_index", outputIndex)
				_ = itemDoneDoc.Set("item.id", fmt.Sprintf("ctc_%s", callID))
				_ = itemDoneDoc.Set("item.status", toolStatus)
				_ = itemDoneDoc.Set("item.input", input)
				_ = itemDoneDoc.Set("item.call_id", callID)
				st.ToolIndex.applyIdentityDoc(itemDoneDoc, st.FuncNames[key], "item")
				out = append(out, emitRespEvent("response.output_item.done", itemDoneDoc.Bytes()))
				st.FuncItemDone[key] = true
				st.FuncArgsDone[key] = true
				continue
			}

			fcDoneDoc := ojson.NewDocString(`{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`)
			_ = fcDoneDoc.Set("sequence_number", nextSeq())
			_ = fcDoneDoc.Set("item_id", fmt.Sprintf("fc_%s", callID))
			_ = fcDoneDoc.Set("output_index", outputIndex)
			_ = translatorcommon.SetStringWithoutHTMLEscapeDoc(fcDoneDoc, "arguments", args)
			out = append(out, emitRespEvent("response.function_call_arguments.done", fcDoneDoc.Bytes()))

			itemDoneDoc := ojson.NewDocString(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`)
			_ = itemDoneDoc.Set("sequence_number", nextSeq())
			_ = itemDoneDoc.Set("output_index", outputIndex)
			_ = itemDoneDoc.Set("item.id", fmt.Sprintf("fc_%s", callID))
			_ = itemDoneDoc.Set("item.status", toolStatus)
			_ = translatorcommon.SetStringWithoutHTMLEscapeDoc(itemDoneDoc, "item.arguments", args)
			_ = itemDoneDoc.Set("item.call_id", callID)
			st.ToolIndex.applyIdentityDoc(itemDoneDoc, st.FuncNames[key], "item")
			out = append(out, emitRespEvent("response.output_item.done", itemDoneDoc.Bytes()))
			st.FuncItemDone[key] = true
			st.FuncArgsDone[key] = true
		}
	}

	if isDone {
		finalizeOpenItems()
		if st.ToolInputError() != nil {
			return out
		}
		hasActiveUnfinishedTool := false
		for key := range st.FuncItemAdded {
			if !st.FuncItemDone[key] {
				hasActiveUnfinishedTool = true
				break
			}
		}
		if hasActiveUnfinishedTool {
			return out
		}
		if len(st.MsgItemAdded) == 0 && len(st.FuncItemAdded) == 0 {
			return out
		}
		st.CompletedEmitted = true
		out = append(out, buildResponsesCompletedEvent(st, nextSeq))
		return out
	}

	// choices[].delta content / tool_calls / reasoning_content
	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() {
		choices.ForEach(func(_, choice ojson.Result) bool {
			idx := int(choice.Get("index").Int())
			delta := choice.Get("delta")
			if delta.Exists() {
				// reasoning_content (OpenAI reasoning incremental text)
				rc := delta.Get("reasoning_content")
				if !rc.Exists() || rc.String() == "" {
					rc = delta.Get("reasoning")
				}
				if rc.Exists() && rc.String() != "" {
					// On first appearance, add reasoning item and part
					if st.ReasoningID == "" {
						st.ReasoningID = fmt.Sprintf("rs_%s_%d", st.ResponseID, idx)
						st.ReasoningIndex = allocOutputIndex()
						itemDoc := ojson.NewDocString(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","summary":[]}}`)
						_ = itemDoc.Set("sequence_number", nextSeq())
						_ = itemDoc.Set("output_index", st.ReasoningIndex)
						_ = itemDoc.Set("item.id", st.ReasoningID)
						out = append(out, emitRespEvent("response.output_item.added", itemDoc.Bytes()))
						partDoc := ojson.NewDocString(`{"type":"response.reasoning_summary_part.added","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
						_ = partDoc.Set("sequence_number", nextSeq())
						_ = partDoc.Set("item_id", st.ReasoningID)
						_ = partDoc.Set("output_index", st.ReasoningIndex)
						out = append(out, emitRespEvent("response.reasoning_summary_part.added", partDoc.Bytes()))
					}
					// Append incremental text to reasoning buffer
					st.ReasoningBuf.WriteString(rc.String())
					msgDoc := ojson.NewDocString(`{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"delta":""}`)
					_ = msgDoc.Set("sequence_number", nextSeq())
					_ = msgDoc.Set("item_id", st.ReasoningID)
					_ = msgDoc.Set("output_index", st.ReasoningIndex)
					_ = msgDoc.Set("delta", rc.String())
					out = append(out, emitRespEvent("response.reasoning_summary_text.delta", msgDoc.Bytes()))
				}

				if c := delta.Get("content"); c.Exists() && c.String() != "" {
					// Ensure the message item and its first content part are announced before any text deltas
					if st.ReasoningID != "" {
						stopReasoning(st.ReasoningBuf.String())
						st.ReasoningBuf.Reset()
					}
					if _, exists := st.MsgOutputIx[idx]; !exists {
						st.MsgOutputIx[idx] = allocOutputIndex()
					}
					msgOutputIndex := st.MsgOutputIx[idx]
					if !st.MsgItemAdded[idx] {
						itemDoc := ojson.NewDocString(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`)
						_ = itemDoc.Set("sequence_number", nextSeq())
						_ = itemDoc.Set("output_index", msgOutputIndex)
						_ = itemDoc.Set("item.id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
						out = append(out, emitRespEvent("response.output_item.added", itemDoc.Bytes()))
						st.MsgItemAdded[idx] = true
					}
					if !st.MsgContentAdded[idx] {
						partDoc := ojson.NewDocString(`{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
						_ = partDoc.Set("sequence_number", nextSeq())
						_ = partDoc.Set("item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
						_ = partDoc.Set("output_index", msgOutputIndex)
						_ = partDoc.Set("content_index", 0)
						out = append(out, emitRespEvent("response.content_part.added", partDoc.Bytes()))
						st.MsgContentAdded[idx] = true
					}

					msgDoc := ojson.NewDocString(`{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`)
					_ = msgDoc.Set("sequence_number", nextSeq())
					_ = msgDoc.Set("item_id", fmt.Sprintf("msg_%s_%d", st.ResponseID, idx))
					_ = msgDoc.Set("output_index", msgOutputIndex)
					_ = msgDoc.Set("content_index", 0)
					_ = msgDoc.Set("delta", c.String())
					out = append(out, emitRespEvent("response.output_text.delta", msgDoc.Bytes()))
					// aggregate for response.output
					if st.MsgTextBuf[idx] == nil {
						st.MsgTextBuf[idx] = &strings.Builder{}
					}
					st.MsgTextBuf[idx].WriteString(c.String())
				}

				// tool calls
				if tcs := delta.Get("tool_calls"); tcs.Exists() && tcs.IsArray() && len(tcs.Array()) > 0 {
					if st.ReasoningID != "" {
						stopReasoning(st.ReasoningBuf.String())
						st.ReasoningBuf.Reset()
					}
					// Before emitting any function events, if a message is open for this index,
					// close its text/content to match Codex expected ordering.
					emitMessageItemDone(idx)

					tcs.ForEach(func(_, tc ojson.Result) bool {
						toolIndex := int(tc.Get("index").Int())
						key := toolStateKey(idx, toolIndex)
						if st.FuncArgsBuf[key] == nil {
							st.FuncArgsBuf[key] = &strings.Builder{}
							st.FuncOutputIx[key] = allocOutputIndex()
						}
						newID, newName := tc.Get("id").String(), st.ToolIndex.canonicalName(tc.Get("function.name").String())
						oldID, oldName := st.FuncCallIDs[key], st.ToolIndex.canonicalName(st.FuncNames[key])
						// Retain conflicting nonempty IDs until the winning tool is known.
						if newID != "" && oldID != "" && newID != oldID {
							st.FuncIdentityConflicts[key] = true
						}
						if st.ToolIndex.isApplyPatch(oldName) || st.ToolIndex.isApplyPatch(newName) {
							if st.FuncIdentityConflicts[key] || (newName != "" && oldName != "" && newName != oldName) {
								failToolInput(fmt.Errorf("conflicting apply_patch call identity"))
								return false
							}
						}
						if newCallID := tc.Get("id").String(); newCallID != "" && st.FuncCallIDs[key] == "" {
							st.FuncCallIDs[key] = strings.Clone(newCallID)
						}
						nameChunk := tc.Get("function.name").String()
						if nameChunk != "" && !st.FuncItemAdded[key] {
							st.FuncNames[key] = strings.Clone(nameChunk)
						}

						if args := tc.Get("function.arguments"); args.Exists() && args.String() != "" {
							st.FuncArgsBuf[key].WriteString(args.String())
						}
						emitToolItem(key, false)
						emitPendingFunctionArgs(key)
						return st.ToolInputError() == nil
					})
				}
			}

			if st.ToolInputError() != nil {
				return false
			}

			// finish_reason triggers item-level finalization. The terminal event is
			// deferred until [DONE] or transport finalization so late usage-only chunks
			// can still populate response.usage.
			if fr := choice.Get("finish_reason"); fr.Exists() && fr.String() != "" {
				st.FinishReason = strings.Clone(fr.String())
				finalizeOpenItems()
				st.CompletionPending = st.canFinalizeResponse()
			}

			return st.ToolInputError() == nil
		})
	}

	return out
}

// ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream builds a single Responses JSON
// from a non-streaming OpenAI Chat Completions response.
func ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	root := util.ParseGJSONBytesNoCopy(rawJSON)
	requestForNamespace := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
	toolIndex := newResponsesToolIndex(util.ParseGJSONBytesNoCopy(requestForNamespace))
	st := &oaiToResponsesState{}
	if param != nil {
		*param = st
	}

	finishReason := root.Get("choices.0.finish_reason").String()
	incompleteDetails, isIncomplete := incompleteByFinishReason(finishReason)

	respStatus := "completed"
	if isIncomplete {
		respStatus = "incomplete"
	}

	// Basic response scaffold
	respDoc := ojson.NewDocString(`{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"incomplete_details":null}`)
	_ = respDoc.Set("status", respStatus)
	if isIncomplete {
		_ = respDoc.SetRawBytes("incomplete_details", incompleteDetails)
	}

	// id: use provider id if present, otherwise synthesize
	id := root.Get("id").String()
	if id == "" {
		id = fmt.Sprintf("resp_%x_%d", time.Now().UnixNano(), atomic.AddUint64(&responseIDCounter, 1))
	}
	_ = respDoc.Set("id", id)

	// created_at: map from chat.completion created
	created := root.Get("created").Int()
	if created == 0 {
		created = time.Now().Unix()
	}
	_ = respDoc.Set("created_at", created)

	// Echo request fields when available (aligns with streaming path behavior)
	echo := responsesRequestEcho{}
	if len(requestRawJSON) > 0 {
		echo = extractResponsesRequestEcho(util.ParseGJSONBytesNoCopy(requestRawJSON))
		// Also support max_tokens from chat completion style
		if !echo.hasMaxOutputTokens && echo.hasMaxTokens {
			echo.maxOutputTokens, echo.hasMaxOutputTokens = echo.maxTokens, true
		}
	}
	if !echo.hasModel {
		// Fallback model from response
		if v := root.Get("model"); v.Exists() {
			echo.model, echo.hasModel = v.String(), true
		}
	}
	echo.applyDoc(respDoc, "")

	// Build output list from choices[...]
	var outputItems [][]byte
	// Detect and capture reasoning content if present (with fallback to reasoning)
	rc := ojson.GetBytesNoCopy(rawJSON, "choices.0.message.reasoning_content")
	if !rc.Exists() || rc.String() == "" {
		rc = ojson.GetBytesNoCopy(rawJSON, "choices.0.message.reasoning")
	}
	rcText := rc.String()
	includeReasoning := rcText != ""
	if !includeReasoning {
		includeReasoning = echo.hasReasoning
	}
	if includeReasoning {
		rid := id
		if strings.HasPrefix(rid, "resp_") {
			rid = strings.TrimPrefix(rid, "resp_")
		}
		// Prefer summary_text from reasoning_content; encrypted_content is optional
		reasoningItemDoc := ojson.NewDocString(`{"id":"","type":"reasoning","encrypted_content":"","summary":[]}`)
		_ = reasoningItemDoc.Set("id", fmt.Sprintf("rs_%s", rid))
		if rcText != "" {
			_ = reasoningItemDoc.Set("summary.0.type", "summary_text")
			_ = reasoningItemDoc.Set("summary.0.text", rcText)
		}
		outputItems = append(outputItems, reasoningItemDoc.Bytes())
	}

	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() {
		choices.ForEach(func(_, choice ojson.Result) bool {
			msg := choice.Get("message")
			if msg.Exists() {
				// Text message part
				if c := msg.Get("content"); c.Exists() && c.String() != "" {
					itemStatus := "completed"
					if isIncomplete {
						itemStatus = "incomplete"
					}
					itemDoc := ojson.NewDocString(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
					_ = itemDoc.Set("id", fmt.Sprintf("msg_%s_%d", id, int(choice.Get("index").Int())))
					_ = itemDoc.Set("status", itemStatus)
					_ = itemDoc.Set("content.0.text", c.String())
					outputItems = append(outputItems, itemDoc.Bytes())
				}

				// Function/tool calls
				if tcs := msg.Get("tool_calls"); tcs.Exists() && tcs.IsArray() {
					customToolNames := toolIndex.custom
					tcs.ForEach(func(tcIndex, tc ojson.Result) bool {
						callID := tc.Get("id").String()
						if callID == "" {
							// Providers may omit tool_call ids; synthesize one so the
							// function_call item stays usable for Codex round-trips.
							callID = fmt.Sprintf("call_%s_%d_%d", id, choice.Get("index").Int(), tcIndex.Int())
						}
						name := toolIndex.canonical(tc.Get("function.name").String())
						args := tc.Get("function.arguments").String()
						toolStatus := "completed"
						if isIncomplete {
							toolStatus = "incomplete"
						}
						if toolIndex.isShell(name) {
							item, errShellCallItem := shellCallItem(callID, args, toolStatus)
							if errShellCallItem != nil {
								st.SetToolInputError(errShellCallItem)
								return false
							}
							outputItems = append(outputItems, item)
							return true
						}
						if _, isCustomTool := customToolNames[name]; isCustomTool {
							itemDoc := ojson.NewDocString(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
							_ = itemDoc.Set("id", fmt.Sprintf("ctc_%s", callID))
							_ = itemDoc.Set("status", toolStatus)
							input := ""
							if toolIndex.isApplyPatch(name) {
								patchCall := &translatorcommon.ApplyPatchCallState{}
								_, fullInput, errFinishArguments := patchCall.FinishArguments(args)
								if errFinishArguments != nil {
									st.SetToolInputError(errFinishArguments)
									return false
								}
								input = fullInput
							} else {
								input = unwrapCustomToolInput(args)
							}
							_ = itemDoc.Set("input", input)
							_ = itemDoc.Set("call_id", callID)
							toolIndex.applyIdentityDoc(itemDoc, name, "")
							outputItems = append(outputItems, itemDoc.Bytes())
							return true
						}
						itemDoc := ojson.NewDocString(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
						_ = itemDoc.Set("id", fmt.Sprintf("fc_%s", callID))
						_ = itemDoc.Set("status", toolStatus)
						_ = translatorcommon.SetStringWithoutHTMLEscapeDoc(itemDoc, "arguments", args)
						_ = itemDoc.Set("call_id", callID)
						toolIndex.applyIdentityDoc(itemDoc, name, "")
						outputItems = append(outputItems, itemDoc.Bytes())
						return true
					})
				}
			}
			return st.ToolInputError() == nil
		})
	}
	if st.ToolInputError() != nil {
		return []byte(ojson.GetBytesNoCopy(responsesToolInputFailure(id, 0, st.ToolInputError()), "response").Raw)
	}
	if len(outputItems) > 0 {
		_ = respDoc.SetRawBytes("output", translatorcommon.JoinRawArray(outputItems))
	}

	// usage mapping
	if usage := root.Get("usage"); usage.Exists() {
		// Map common tokens
		if usage.Get("prompt_tokens").Exists() || usage.Get("completion_tokens").Exists() || usage.Get("total_tokens").Exists() {
			_ = respDoc.Set("usage.input_tokens", usage.Get("prompt_tokens").Int())
			if d := usage.Get("prompt_tokens_details.cached_tokens"); d.Exists() {
				_ = respDoc.Set("usage.input_tokens_details.cached_tokens", d.Int())
			}
			_ = respDoc.Set("usage.output_tokens", usage.Get("completion_tokens").Int())
			// Chat Completions upstreams report reasoning under completion_tokens_details;
			// Responses-native upstreams report it under output_tokens_details. Accept both.
			if d := usage.Get("output_tokens_details.reasoning_tokens"); d.Exists() {
				_ = respDoc.Set("usage.output_tokens_details.reasoning_tokens", d.Int())
			} else if d := usage.Get("completion_tokens_details.reasoning_tokens"); d.Exists() {
				_ = respDoc.Set("usage.output_tokens_details.reasoning_tokens", d.Int())
			}
			_ = respDoc.Set("usage.total_tokens", usage.Get("total_tokens").Int())
		} else {
			// Fallback to raw usage object if structure differs
			_ = respDoc.Set("usage", usage.Value())
		}
	}

	return respDoc.Bytes()
}

func (st *oaiToResponsesState) canFinalizeResponse() bool {
	if st.ToolInputError() != nil || st.FinishReason == "" || (len(st.MsgItemAdded) == 0 && len(st.FuncItemAdded) == 0) {
		return false
	}
	for idx := range st.MsgItemAdded {
		if !st.MsgItemDone[idx] {
			return false
		}
	}
	for key := range st.FuncItemAdded {
		if !st.FuncItemDone[key] {
			return false
		}
	}
	return st.ReasoningID == ""
}

// CanFinalizeResponseStream reports whether EOF can safely synthesize the source terminator.
func (st *oaiToResponsesState) CanFinalizeResponseStream() bool {
	return st.ToolInputError() == nil && !st.CompletedEmitted && st.CompletionPending
}

// FinalizeToolInput rejects a patch-enabled stream lacking its source terminator.
func (st *oaiToResponsesState) FinalizeToolInput() [][]byte {
	if st.ToolInputError() != nil || st.CompletedEmitted {
		return nil
	}
	if st.CanFinalizeResponseStream() {
		return nil
	}
	enabled := false
	if st.ToolIndex != nil {
		for name := range st.ToolIndex.byChat {
			if st.ToolIndex.isApplyPatch(name) {
				enabled = true
				break
			}
		}
	}
	if !enabled {
		return nil
	}
	st.SetToolInputError(fmt.Errorf("upstream apply_patch stream ended before protocol completion"))
	st.Seq++
	return [][]byte{emitRespEvent("response.failed", translatorcommon.ApplyPatchFailure(st.ResponseID, st.Seq))}
}
