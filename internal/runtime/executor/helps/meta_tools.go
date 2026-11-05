package helps

import (
	"fmt"

	"github.com/MoeForge/moego/ojson"
)

// SanitizeMetaWebSearchTools strips unsupported fields from web_search tool definitions
// for Meta Muse upstreams. Specifically, Meta rejects search_content_types when tool type
// is "web_search" (it is only accepted on "web_search_preview" tools).
func SanitizeMetaWebSearchTools(body []byte) []byte {
	doc := ojson.NewDoc(body)
	if !SanitizeMetaWebSearchToolsDoc(doc) {
		return body
	}
	return doc.Bytes()
}

// SanitizeMetaWebSearchToolsDoc strips unsupported search fields in place.
func SanitizeMetaWebSearchToolsDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	var pathsToDelete []string
	tools.ForEach(func(index, tool ojson.Result) bool {
		if tool.Get("type").String() == "web_search" && tool.Get("search_content_types").Exists() {
			pathsToDelete = append(pathsToDelete, fmt.Sprintf("tools.%d.search_content_types", index.Int()))
		}
		if tool.Get("type").String() == "namespace" {
			subtools := tool.Get("tools")
			if subtools.Exists() && subtools.IsArray() {
				subtools.ForEach(func(subIndex, subtool ojson.Result) bool {
					if subtool.Get("type").String() == "web_search" && subtool.Get("search_content_types").Exists() {
						pathsToDelete = append(pathsToDelete, fmt.Sprintf("tools.%d.tools.%d.search_content_types", index.Int(), subIndex.Int()))
					}
					return true
				})
			}
		}
		return true
	})
	changed := false
	for _, p := range pathsToDelete {
		if errDelete := doc.Delete(p); errDelete == nil {
			changed = true
		}
	}
	return changed
}
