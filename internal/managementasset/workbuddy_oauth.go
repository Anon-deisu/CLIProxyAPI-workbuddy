package managementasset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

var workBuddyLocaleAnchor = regexp.MustCompile("(?:\"kimi_oauth_polling_error\"|kimi_oauth_polling_error):(?:`[^`]*`|\"(?:\\\\.|[^\"\\\\])*\")")

const workBuddyIcon = `data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='7' fill='%2300b88a'/%3E%3Cpath d='m6 9 4 15 6-11 6 11 4-15' fill='none' stroke='white' stroke-width='3' stroke-linejoin='round'/%3E%3C/svg%3E`

// AddWorkBuddyOAuthToManagementHTML extends the native OAuth provider list.
func AddWorkBuddyOAuthToManagementHTML(data []byte) ([]byte, error) {
	if bytes.Contains(data, []byte("id:`workbuddy`,titleKey:")) {
		return data, nil
	}
	anchor := []byte("{kind:`builtin`,id:`kimi`,titleKey:")
	start := bytes.Index(data, anchor)
	if start < 0 {
		return nil, fmt.Errorf("management html WorkBuddy provider anchor not found")
	}
	end := bytes.Index(data[start:], []byte("}}"))
	if end < 0 {
		return nil, fmt.Errorf("management html WorkBuddy provider boundary not found")
	}
	// A self-contained monogram keeps the login cards usable without external assets.
	encodedIcon, err := json.Marshal(workBuddyIcon)
	if err != nil {
		return nil, err
	}
	providers := fmt.Sprintf(",{kind:`builtin`,id:`workbuddy`,titleKey:`auth_login.workbuddy_oauth_title`,icon:%s},{kind:`builtin`,id:`workbuddy-ai`,titleKey:`auth_login.workbuddy_ai_oauth_title`,icon:%s}", encodedIcon, encodedIcon)
	data = insertBytes(data, start+end+2, []byte(providers))
	matches := workBuddyLocaleAnchor.FindAllIndex(data, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("management html WorkBuddy locale anchor not found")
	}
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		chinese := bytes.Contains(data[match[0]:match[1]], []byte("检查")) || bytes.Contains(data[match[0]:match[1]], []byte("檢查"))
		labels := make(map[string]string)
		for _, provider := range []struct{ prefix, name, cnName string }{{"workbuddy", "WorkBuddy", "WorkBuddy 国内站"}, {"workbuddy_ai", "WorkBuddy AI", "WorkBuddy 国际站"}} {
			values := map[string]string{
				"oauth_title": provider.name + " OAuth", "oauth_button": "Sign in to " + provider.name,
				"oauth_hint": "", "oauth_url_label": "Authorization URL:", "open_link": "Open link", "copy_link": "Copy link",
				"oauth_status_waiting": "Waiting for authorization...", "oauth_status_success": "Authentication successful!",
				"oauth_status_error": "Authentication failed:", "oauth_start_error": "Failed to start authorization:", "oauth_polling_error": "Failed to check authorization:",
			}
			if chinese {
				values = map[string]string{
					"oauth_title": provider.cnName, "oauth_button": "登录 " + provider.cnName, "oauth_hint": "",
					"oauth_url_label": "授权链接：", "open_link": "打开链接", "copy_link": "复制链接",
					"oauth_status_waiting": "等待授权中...", "oauth_status_success": "认证成功！",
					"oauth_status_error": "认证失败：", "oauth_start_error": "启动授权失败：", "oauth_polling_error": "检查认证状态失败：",
				}
			}
			for suffix, value := range values {
				labels[provider.prefix+"_"+suffix] = value
			}
		}
		encoded, errMarshal := json.Marshal(labels)
		if errMarshal != nil {
			return nil, errMarshal
		}
		addition := append([]byte(","), encoded[1:len(encoded)-1]...)
		data = insertBytes(data, match[1], addition)
	}
	return data, nil
}

// The native registry shares quota state between the quota page and auth-file cards.
const workBuddyQuotaRuntime = `
function cpaWorkBuddyQuota(provider) {
  const international = provider === 'workbuddy-ai';
  const store = international ? 'workbuddyAIQuota' : 'workbuddyQuota';
  return {
    type: provider,
    i18nPrefix: 'workbuddy_quota',
    filterFn: file => hm(file) === provider && !Sm(file) && !YD(file),
    fetchQuota: async (file, t) => {
      const index = $g(file.auth_index ?? file.authIndex);
      if (!index) throw Error(t('workbuddy_quota.missing_auth_index'));
      return sp.get('/credentials/workbuddy-quota', {params: {auth_index: index}});
    },
    storeSelector: state => state[store],
    storeSetter: international ? 'setWorkBuddyAIQuota' : 'setWorkBuddyQuota',
    buildLoadingState: () => ({status: 'loading', credits: null}),
    buildSuccessState: data => ({...data, status: 'success'}),
    buildErrorState: (error, errorStatus) => ({status: 'error', credits: null, error, errorStatus}),
    Body: cpaWorkBuddyQuotaBody
  };
}
function cpaWorkBuddyQuotaBody({quota, classes}) {
  const {t, i18n} = H();
  const credits = quota.credits;
  const format = value => new Intl.NumberFormat(i18n.resolvedLanguage, {maximumFractionDigits: 2}).format(value);
  const row = (label, value) => U.jsxs('div', {className: classes.quotaRow, children: [
    U.jsxs('div', {className: classes.quotaRowHeader, children: [
      U.jsx('span', {className: classes.quotaModel, children: label}),
      U.jsx('span', {className: classes.quotaAmount, children: value})
    ]})
  ]}, label);
  return U.jsxs(U.Fragment, {children: [
    U.jsxs('div', {className: classes.codexPlan, children: [
      U.jsx('span', {className: classes.codexPlanLabel, children: t('workbuddy_quota.plan')}),
      U.jsx('span', {className: classes.codexPlanValue, children: quota.plan})
    ]}),
    credits ? U.jsxs(U.Fragment, {children: [
      U.jsxs('div', {className: classes.quotaRow, children: [
        U.jsxs('div', {className: classes.quotaRowHeader, children: [
          U.jsx('span', {className: classes.quotaModel, children: t('workbuddy_quota.remaining')}),
          U.jsxs('div', {className: classes.quotaMeta, children: [
            U.jsx('span', {className: classes.quotaPercent, children: format(credits.remaining_percent) + '%'}),
            U.jsx('span', {className: classes.quotaAmount, children: format(credits.remaining) + ' / ' + format(credits.total)})
          ]})
        ]}),
        U.jsx(pO, {percent: credits.remaining_percent, classes, index: 0})
      ]}),
      row(t('workbuddy_quota.used'), format(credits.used)),
      credits.resets_at && row(t('workbuddy_quota.resets_at'), new Date(credits.resets_at).toLocaleString(i18n.resolvedLanguage))
    ]}) : U.jsx('div', {className: classes.quotaMessage, children: t('workbuddy_quota.empty_data')})
  ]});
}
function cpaWorkBuddyCredits({className}) {
  const connected = Mm(state => state.connectionStatus === 'connected');
  const generation = Om(state => state.cacheGeneration);
  const [summary, setSummary] = y.useState(null);
  const [failed, setFailed] = y.useState(false);
  y.useEffect(() => {
    let active = true, pending = false;
    setSummary(null);
    if (!connected) return;
    const load = async () => {
      if (document.hidden || pending) return;
      pending = true;
      try {
        const data = await sp.get('/observability/workbuddy-credits');
        if (active) { setSummary(data); setFailed(false); }
      } catch (error) {
        if (active) setFailed(true);
      } finally { pending = false; }
    };
    load();
    const timer = window.setInterval(load, 30000);
    document.addEventListener('visibilitychange', load);
    return () => { active = false; window.clearInterval(timer); document.removeEventListener('visibilitychange', load); };
  }, [connected, generation]);
  if (summary?.available && summary.remaining === 0) return null;
  const value = summary?.available ? new Intl.NumberFormat('zh-CN', {maximumFractionDigits: 2, useGrouping: false}).format(summary.remaining) : '--';
  const partial = summary && (summary.failed > 0 || summary.known < summary.accounts);
  const title = failed ? '积分汇总读取失败，保留最近结果' : partial ? '部分凭证未刷新成功，保留最近一次成功余额' : summary?.updated_at ? new Date(summary.updated_at).toLocaleString() : '';
  return U.jsx('span', {className, 'data-cpa-workbuddy-credits': true, title, children: 'workbuddy剩余积分:' + value});
}
function cpaAllQuotaEntries(files) {
  return files.flatMap(file => {
    const type = OM.find(type => Wk[type].filterFn({...file, disabled: false}));
    return type ? [{type, file}] : [];
  });
}
async function cpaLoadQuotaEntries(entries, t, generation) {
  const groups = new Map();
  for (const {type, file} of entries) {
    if (!groups.has(type)) groups.set(type, {files: [], results: []});
    groups.get(type).files.push(file);
  }
  for (const [type, group] of groups) {
    const adapter = Wk[type];
    Gk(adapter)(state => {
      const next = {...state};
      for (const file of group.files) Am(generation, () => { next[wm(file)] = adapter.buildLoadingState(); }, file.name);
      return next;
    });
  }
  let next = 0;
  const worker = async () => {
    while (next < entries.length) {
      const {type, file} = entries[next++];
      if (!Am(generation, () => {}, file.name)) continue;
      const adapter = Wk[type], key = wm(file);
      let result;
      try { result = adapter.buildSuccessState(await adapter.fetchQuota(file, t)); }
      catch (error) { result = adapter.buildErrorState(error instanceof Error ? error.message : t('common.unknown_error'), M_(error)); }
      groups.get(type).results.push({file, key, result});
    }
  };
  await Promise.all(Array.from({length: Math.min(4, entries.length)}, worker));
  for (const [type, group] of groups) {
    Gk(Wk[type])(state => {
      const next = {...state};
      for (const {file, key, result} of group.results) Am(generation, () => { next[key] = result; }, file.name);
      return next;
    });
  }
}
`

const workBuddyQuotaBatchRuntime = `function pN() {
  const {t} = H();
  const [batchLoading, setLoading] = y.useState(false);
  const pending = y.useRef(false);
  const loadQuota = y.useCallback(async entries => {
    if (pending.current || entries.length === 0) return;
    pending.current = true;
    setLoading(true);
    try { await cpaLoadQuotaEntries(entries, t, km()); }
    finally { pending.current = false; setLoading(false); }
  }, [t]);
  return {batchLoading, loadQuota};
}`

const workBuddyRefreshEveryRuntime = `let [cpaAllLoading,cpaSetAllLoading]=y.useState(false),cpaAllPending=y.useRef(false);
let cpaRefreshEvery=y.useCallback(async()=>{
  if(b||a||le||cpaAllPending.current)return;
  cpaAllPending.current=true;cpaSetAllLoading(true);c('');
  const generation=km();
  try{
    const response=await Ry.list();
    if(!Am(generation,()=>{}))return;
    const files=response?.files||[];
    i(files);C(x);
    await ue(cpaAllQuotaEntries(files));
  }catch(error){Am(generation,()=>c(error instanceof Error?error.message:e('notification.refresh_failed')));}
  finally{cpaAllPending.current=false;cpaSetAllLoading(false);}
},[b,a,le,ue,x,e]);`

var workBuddyQuotaLocaleAnchor = regexp.MustCompile(`(?:"kimi_quota"|kimi_quota):\{`)

// AddWorkBuddyQuotaToManagementHTML registers both sites in the existing quota UI.
func AddWorkBuddyQuotaToManagementHTML(data []byte) ([]byte, error) {
	if bytes.Contains(data, []byte("function cpaWorkBuddyQuota(")) {
		return data, nil
	}
	icon, err := json.Marshal(workBuddyIcon)
	if err != nil {
		return nil, err
	}
	runtime := workBuddyQuotaRuntime + fmt.Sprintf("TD.workbuddy=%s;TD['workbuddy-ai']=%s;", icon, icon)
	patches := []struct{ old, replacement string }{
		{"xaiQuota:{}", "xaiQuota:{},workbuddyQuota:{},workbuddyAIQuota:{}"},
		{"setXaiQuota:t=>", "setWorkBuddyQuota:t=>e(e=>({workbuddyQuota:Dm(t,e.workbuddyQuota)})),setWorkBuddyAIQuota:t=>e(e=>({workbuddyAIQuota:Dm(t,e.workbuddyAIQuota)})),setXaiQuota:t=>"},
		{"xaiQuota:i(e.xaiQuota)", "xaiQuota:i(e.xaiQuota),workbuddyQuota:i(e.workbuddyQuota),workbuddyAIQuota:i(e.workbuddyAIQuota)"},
		{"var Wk={", runtime + "var Wk={workbuddy:cpaWorkBuddyQuota('workbuddy'),\"workbuddy-ai\":cpaWorkBuddyQuota('workbuddy-ai'),"},
		{"OM=[`claude`,`antigravity`,`codex`,`xai`,`kimi`,`devin`,`meta`]", "OM=[`claude`,`antigravity`,`codex`,`xai`,`kimi`,`devin`,`meta`,`workbuddy`,`workbuddy-ai`]"},
		{"AM={antigravity:", "AM={workbuddy:Wk.workbuddy.filterFn,\"workbuddy-ai\":Wk['workbuddy-ai'].filterFn,antigravity:"},
		{"gD=new Set([", "gD=new Set([`workbuddy`,`workbuddy-ai`,"},
		{"PD=(e,t)=>{let n=DD(t)", "PD=(e,t)=>{if(t==='workbuddy'||t==='workbuddy-ai')return e(t==='workbuddy'?'workbuddy_quota.site_cn':'workbuddy_quota.site_ai');let n=DD(t)"},
		{"d=Om(e=>n===`antigravity`?e.antigravityQuota[u]:n===`claude`?e.claudeQuota[u]:n===`codex`?e.codexQuota[u]:n===`devin`?e.devinQuota[u]:n===`kimi`?e.kimiQuota[u]:n===`meta`?e.metaQuota[u]:n===`xai`?e.xaiQuota[u]:Yk(n))", "d=Om(e=>l.storeSelector(e)[u])"},
		{"b||n===`devin`", "b||n===`devin`||n===`workbuddy`||n===`workbuddy-ai`"},
		{"n===`devin`&&(0,U.jsxs)(W", "(n===`devin`||n===`workbuddy`||n===`workbuddy-ai`)&&(0,U.jsxs)(W"},
		{"M=Om(e=>e.xaiQuota),N=", "M=Om(e=>e.xaiQuota),cpaWB=Om(e=>e.workbuddyQuota),cpaWBAI=Om(e=>e.workbuddyAIQuota),N="},
		{"meta:j,xai:M}),[E,D,O,k,A,j,M])", "meta:j,xai:M,workbuddy:cpaWB,\"workbuddy-ai\":cpaWBAI}),[E,D,O,k,A,j,M,cpaWB,cpaWBAI])"},
		{"if(!n||n.status!==`success`)return[];if(e===`claude`", "if(!n||n.status!==`success`)return[];if(e==='workbuddy'||e==='workbuddy-ai')return t.credits?.resets_at?[{rowId:'credits',atMs:Date.parse(t.credits.resets_at),kind:'window'}]:[];if(e===`claude`"},
		{"(0,U.jsx)(`span`,{className:K.statHint,children:e.hint})", "(0,U.jsx)(`span`,{className:K.statHint,children:e.hint}),e.key==='credentials'&&(0,U.jsx)(cpaWorkBuddyCredits,{className:K.statHint})"},
		{"[b,T,x]);(0,y.useEffect)", "[b,T,x]);" + workBuddyRefreshEveryRuntime + "(0,y.useEffect)"},
		{"uN(V,b||a||le||!!s||S!==x,ue);let _e=!b&&!a&&S===x", "uN(V,b||a||le||cpaAllLoading||!!s||S!==x,ue);let _e=!b&&!a&&!le&&!cpaAllLoading&&S===x"},
		{"refreshing:a||le,disableControls:b,onRefreshAll:ge", "refreshing:a||le||cpaAllLoading,disableControls:b,onRefreshAll:ge,onRefreshEveryCredential:cpaRefreshEvery"},
		{"(0,U.jsx)(`div`,{className:EM.actions,\"data-reveal\":!0,children:(0,U.jsxs)(`button`,{type:`button`,className:EM.primaryAction,onClick:o,disabled:a||i,children:[(0,U.jsx)(Cs,{size:14,className:i?EM.spinning:void 0}),s(`quota_management.refresh_all_credentials`)]})})", `(0,U.jsxs)('div',{className:EM.actions,style:{flexWrap:'wrap'},"data-reveal":true,children:[(0,U.jsxs)('button',{type:'button',className:EM.primaryAction+' cpa-quota-refresh-every',style:{background:'var(--danger-color, #dc2626)',borderColor:'var(--danger-color, #dc2626)',color:'#fff'},onClick:e.onRefreshEveryCredential,disabled:a||i,children:[(0,U.jsx)(Cs,{size:14,className:i?EM.spinning:void 0}),'刷新"全部"凭证']}),(0,U.jsxs)('button',{type:'button',className:EM.primaryAction,onClick:o,disabled:a||i,children:[(0,U.jsx)(Cs,{size:14,className:i?EM.spinning:void 0}),s('quota_management.refresh_all_credentials')]})]})`},
	}
	for _, patch := range patches {
		if !bytes.Contains(data, []byte(patch.old)) {
			return nil, fmt.Errorf("management html WorkBuddy quota anchor not found: %s", patch.old)
		}
		data = bytes.ReplaceAll(data, []byte(patch.old), []byte(patch.replacement))
	}
	batchStart := bytes.Index(data, []byte("function pN(){"))
	if batchStart < 0 {
		return nil, fmt.Errorf("management html quota batch anchor not found")
	}
	batchEnd := bytes.Index(data[batchStart:], []byte("var mN="))
	if batchEnd < 0 {
		return nil, fmt.Errorf("management html quota batch boundary not found")
	}
	data = bytes.Replace(data, data[batchStart:batchStart+batchEnd], []byte(workBuddyQuotaBatchRuntime), 1)
	matches := workBuddyQuotaLocaleAnchor.FindAllIndex(data, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("management html WorkBuddy quota locale anchor not found")
	}
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		sample := data[match[0]:min(len(data), match[1]+120)]
		labels := map[string]string{
			"site_cn": "WorkBuddy CN", "site_ai": "WorkBuddy AI", "plan": "Plan", "remaining": "Credits remaining", "used": "Credits used", "resets_at": "Resets at",
			"idle": "Click here to refresh quota", "loading": "Loading quota...", "load_failed": "Failed to load quota: {{message}}", "missing_auth_index": "Auth file missing auth_index",
			"empty_title": "No WorkBuddy accounts", "empty_desc": "", "empty_data": "No active credit package",
		}
		if bytes.Contains(sample, []byte("额度")) || bytes.Contains(sample, []byte("配額")) {
			labels = map[string]string{
				"site_cn": "WorkBuddy 国内站", "site_ai": "WorkBuddy 国际站", "plan": "套餐", "remaining": "剩余积分", "used": "已用积分", "resets_at": "重置时间",
				"idle": "点击此处刷新额度", "loading": "正在加载额度...", "load_failed": "额度获取失败：{{message}}", "missing_auth_index": "认证文件缺少 auth_index",
				"empty_title": "暂无 WorkBuddy 账号", "empty_desc": "", "empty_data": "暂无有效积分套餐",
			}
		}
		encoded, errMarshal := json.Marshal(labels)
		if errMarshal != nil {
			return nil, errMarshal
		}
		addition := append([]byte(`"workbuddy_quota":`), encoded...)
		addition = append(addition, ',')
		data = insertBytes(data, match[0], addition)
	}
	return data, nil
}
