package managementasset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

const qoderIcon = `data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='7' fill='%232b2846'/%3E%3Ccircle cx='15' cy='15' r='8' fill='none' stroke='%23c4b5fd' stroke-width='3'/%3E%3Cpath d='m19 19 7 7' stroke='%23c4b5fd' stroke-width='3'/%3E%3C/svg%3E`

var qoderOAuthLocaleAnchor = regexp.MustCompile("(?:\"kimi_oauth_polling_error\"|kimi_oauth_polling_error):(?:`[^`]*`|\"(?:\\\\.|[^\"\\\\])*\")")
var qoderQuotaLocaleAnchor = regexp.MustCompile(`(?:"kimi_quota"|kimi_quota):\{`)

// AddQoderOAuthToManagementHTML runs after the WorkBuddy OAuth patch.
func AddQoderOAuthToManagementHTML(data []byte) ([]byte, error) {
	if bytes.Contains(data, []byte("id:`qoder`,titleKey:")) && bytes.Contains(data, []byte("id:`qoder-cn`,titleKey:")) {
		return data, nil
	}
	anchor := []byte("{kind:`builtin`,id:`kimi`,titleKey:")
	start := bytes.Index(data, anchor)
	if start < 0 {
		return nil, fmt.Errorf("management html Qoder OAuth provider anchor not found")
	}
	end := bytes.Index(data[start:], []byte("}}"))
	if end < 0 {
		return nil, fmt.Errorf("management html Qoder OAuth provider boundary not found")
	}
	icon, _ := json.Marshal(qoderIcon)
	providers := fmt.Sprintf(",{kind:`builtin`,id:`qoder`,titleKey:`auth_login.qoder_oauth_title`,icon:%s},{kind:`builtin`,id:`qoder-cn`,titleKey:`auth_login.qoder_cn_oauth_title`,icon:%s}", icon, icon)
	data = insertBytes(data, start+end+2, []byte(providers))
	matches := qoderOAuthLocaleAnchor.FindAllIndex(data, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("management html Qoder OAuth locale anchor not found")
	}
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		chinese := bytes.Contains(data[match[0]:match[1]], []byte("检查")) || bytes.Contains(data[match[0]:match[1]], []byte("檢查"))
		labels := make(map[string]string)
		for _, site := range []struct{ prefix, en, zh string }{{"qoder", "Qoder International", "Qoder 国际站"}, {"qoder_cn", "Qoder China", "Qoder 国内站"}} {
			values := map[string]string{
				"oauth_title": site.en, "oauth_button": "Sign in to " + site.en, "oauth_hint": "",
				"oauth_url_label": "Authorization URL:", "open_link": "Open link", "copy_link": "Copy link",
				"oauth_status_waiting": "Waiting for authorization...", "oauth_status_success": "Authentication successful!",
				"oauth_status_error": "Authentication failed:", "oauth_start_error": "Failed to start authorization:", "oauth_polling_error": "Failed to check authorization:",
			}
			if chinese {
				values = map[string]string{
					"oauth_title": site.zh, "oauth_button": "登录 " + site.zh, "oauth_hint": "",
					"oauth_url_label": "授权链接：", "open_link": "打开链接", "copy_link": "复制链接",
					"oauth_status_waiting": "等待授权中...", "oauth_status_success": "认证成功！",
					"oauth_status_error": "认证失败：", "oauth_start_error": "启动授权失败：", "oauth_polling_error": "检查认证状态失败：",
				}
			}
			for suffix, value := range values {
				labels[site.prefix+"_"+suffix] = value
			}
		}
		encoded, errMarshal := json.Marshal(labels)
		if errMarshal != nil {
			return nil, errMarshal
		}
		data = insertBytes(data, match[1], append([]byte(","), encoded[1:len(encoded)-1]...))
	}
	return data, nil
}

// These adapters use the native store, cache generations and request client.
const qoderQuotaRuntime = `
function cpaIsQoder(file) {
  const provider = hm(file);
  return provider === 'qoder' || provider === 'qoder-cn';
}
function cpaQoderIdentity(file, t) {
  const text = value => typeof value === 'string' || typeof value === 'number' ? String(value).trim() : '';
  const name = text(file.nickname) || text(file.label) || text(file.email) || text(file.uid) || text(file.name);
  const uid = text(file.uid);
  const site = t(hm(file) === 'qoder-cn' ? 'qoder_quota.site_cn' : 'qoder_quota.site_intl');
  const secondary = [uid && 'UID: ' + uid, site].filter(Boolean).join(' · ');
  return {primary: name, kind: 'account', secondary, fullName: [name, secondary].join(' · ')};
}
function cpaQoderSearch(file) {
  return cpaIsQoder(file) ? [file.nickname, file.label, file.uid, file.realm, file.organization_name, file.organization_id].filter(value => typeof value === 'string' || typeof value === 'number').map(String) : [];
}
function cpaQoderQuota(provider) {
  const cn = provider === 'qoder-cn';
  const store = cn ? 'qoderCNQuota' : 'qoderQuota';
  return {
    type: provider,
    i18nPrefix: 'qoder_quota',
    filterFn: file => hm(file) === provider && !Sm(file) && !YD(file),
    fetchQuota: async (file, t) => {
      const index = $g(file.auth_index ?? file.authIndex);
      if (!index) throw Error(t('qoder_quota.missing_auth_index'));
      return sp.get('/credentials/qoder-quota', {params: {auth_index: index}});
    },
    storeSelector: state => state[store],
    storeSetter: cn ? 'setQoderCNQuota' : 'setQoderQuota',
    buildLoadingState: () => ({status: 'loading', credits: null}),
    buildSuccessState: data => ({...data, status: 'success'}),
    buildErrorState: (error, errorStatus) => ({status: 'error', credits: null, error, errorStatus}),
    Body: cpaQoderQuotaBody
  };
}
function cpaQoderQuotaBody({quota, classes}) {
  const {t, i18n} = H();
  const number = value => typeof value === 'number' && Number.isFinite(value);
  const format = value => number(value) ? new Intl.NumberFormat(i18n.resolvedLanguage, {maximumFractionDigits: 2}).format(value) : '—';
  const row = (label, value) => U.jsxs('div', {className: classes.quotaRow, children: [
    U.jsxs('div', {className: classes.quotaRowHeader, children: [
      U.jsx('span', {className: classes.quotaModel, children: label}),
      U.jsx('span', {className: classes.quotaAmount, children: value})
    ]})
  ]}, label);
  const pool = (label, credits, key) => {
    const unavailable = credits.available === false;
    const unlimited = credits.unlimited === true || credits.total < 0;
    const percent = number(credits.remaining_percent) ? Math.max(0, Math.min(100, credits.remaining_percent)) : null;
    const value = unavailable ? t('qoder_quota.unavailable') : unlimited ? t('qoder_quota.unlimited') : format(credits.remaining) + ' / ' + format(credits.total);
    return U.jsxs('div', {className: classes.quotaRow, children: [
      row(label, value),
      !unavailable && !unlimited && percent !== null && row(t('qoder_quota.remaining_percent'), format(percent) + '%'),
      !unavailable && !unlimited && percent !== null && U.jsx(pO, {percent, classes}),
      !unavailable && !unlimited && row(t('qoder_quota.used'), format(credits.used)),
      credits.organization_name && row(t('qoder_quota.organization'), credits.organization_name),
      credits.no_expiry !== true && number(credits.expires_at) && credits.expires_at > 0 && row(t('qoder_quota.expires_at'), new Date(credits.expires_at * 1000).toLocaleString(i18n.resolvedLanguage))
    ]}, key);
  };
  const packages = Array.isArray(quota.packages) ? quota.packages : [];
  return U.jsxs(U.Fragment, {children: [
    quota.plan && row(t('qoder_quota.plan'), quota.plan),
    quota.credits && pool(t('qoder_quota.remaining'), quota.credits, 'personal'),
    packages.filter(pack => pack && pack.source !== 'orgResourcePackage').map((pack, index) => {
      const label = pack.source === 'userQuota' ? t('qoder_quota.base') : pack.source === 'addOnQuota' ? t('qoder_quota.addon') : pack.name || t('qoder_quota.package');
      return pool(label, pack, 'package-' + index);
    }),
    quota.shared && pool(t('qoder_quota.shared'), quota.shared, 'shared'),
    !quota.credits && !quota.shared && !packages.length && U.jsx('div', {className: classes.quotaMessage, children: t('qoder_quota.empty_data')})
  ]});
}
function cpaQoderAutoCheckin(file) {
  return (file.qoder_auto_checkin ?? file.metadata?.qoder_auto_checkin) === true;
}
function cpaQoderNumber(value) {
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  if (typeof value === 'string' && value.trim() !== '') {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
}
function cpaQoderCheckinTime(value, locale) {
  if (typeof value === 'boolean' || value === null || value === undefined || value === '') return '';
  const numeric = cpaQoderNumber(value);
  const date = numeric === null ? new Date(String(value)) : new Date(numeric < 1e12 ? numeric * 1000 : numeric);
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString(locale);
}
function cpaQoderCheckinStatusText(result, t) {
  const value = result && typeof result === 'object' ? result : {};
  const status = String(value.status || 'error').trim().toLowerCase();
  const credits = cpaQoderNumber(value.credits);
  const claimedValue = cpaQoderNumber(value.claimed);
  const claimed = value.claimed === true || (claimedValue !== null && claimedValue > 0) || (credits !== null && credits > 0);
  const awarded = claimed ? t('qoder_quota.checkin_status_claimed', {credits: credits === null ? '0' : credits}) : '';
  if ((status === 'success' || status === 'already') && awarded) return awarded;
  const key = {
    success: 'checkin_status_success', already: 'checkin_status_already', skipped: 'checkin_status_skipped',
    blocked: 'checkin_status_blocked', unavailable: 'checkin_status_unavailable', error: 'checkin_status_error'
  }[status] || 'checkin_status_error';
  return [awarded, t('qoder_quota.' + key)].filter(Boolean).join(' · ');
}
function cpaQoderCheckinDisplay(result, t, locale) {
  const value = result && typeof result === 'object' ? result : {};
  let text = cpaQoderCheckinStatusText(value, t);
  const nextAttempt = cpaQoderCheckinTime(value.next_attempt ?? value.nextAttempt, locale);
  if (nextAttempt) text += ' · ' + t('qoder_quota.checkin_next_attempt', {time: nextAttempt});
  return text;
}
function cpaQoderInitialCheckinDisplay(file, t, locale) {
  const metadata = file.metadata && typeof file.metadata === 'object' ? file.metadata : {};
  const raw = file.qoder_checkin_last_result ?? metadata.qoder_checkin_last_result;
  const nextAttempt = file.qoder_checkin_next_attempt ?? metadata.qoder_checkin_next_attempt ?? metadata.next_attempt;
  const lastSuccess = file.qoder_checkin_last_success ?? metadata.qoder_checkin_last_success ?? metadata.last_success;
  if (raw === undefined && nextAttempt === undefined && lastSuccess === undefined) return '';
  const result = raw && typeof raw === 'object' ? {...raw} : {status: raw || 'error'};
  if (nextAttempt !== undefined && result.next_attempt === undefined) result.next_attempt = nextAttempt;
  let text = cpaQoderCheckinDisplay(result, t, locale);
  const successTime = cpaQoderCheckinTime(lastSuccess, locale);
  if (successTime) text += ' · ' + t('qoder_quota.checkin_last_success', {time: successTime});
  return text;
}
async function cpaQoderCheckin(file, t) {
  const index = $g(file.auth_index ?? file.authIndex);
  if (!cpaIsQoder(file) || !index) throw Error(t('qoder_quota.missing_auth_index'));
  const result = await sp.post('/credentials/qoder-checkin', undefined, {params: {auth_index: index}});
  const normalized = result && typeof result === 'object' ? result : {status: 'error'};
  const status = String(normalized.status || 'error').trim().toLowerCase();
  const credits = cpaQoderNumber(normalized.credits);
  const claimed = normalized.claimed === true || (cpaQoderNumber(normalized.claimed) || 0) > 0 || (credits !== null && credits > 0);
  if (status === 'success' || claimed) Om.getState().clearQuotaCache([file.name]);
  return normalized;
}
async function cpaQoderSetAutoCheckin(file, enabled) {
  if (!cpaIsQoder(file)) throw Error('Unsupported account');
  await Ry.patchFields(file.name, {qoder_auto_checkin: enabled === true});
}
function cpaQoderAccountActions({file, disabled}) {
  const {t, i18n} = H();
  const [auto, setAuto] = y.useState(() => cpaQoderAutoCheckin(file));
  const [busy, setBusy] = y.useState(false);
  const [message, setMessage] = y.useState(null);
  const active = y.useRef(false);
  y.useEffect(() => {setAuto(cpaQoderAutoCheckin(file)); setMessage(null);}, [file.name, file.auth_index, file.authIndex, file.qoder_auto_checkin, file.metadata?.qoder_auto_checkin, file.qoder_checkin_last_attempt]);
  const run = async task => {
    if (disabled || active.current) return;
    active.current = true; setBusy(true); setMessage(null);
    try { await task(); } catch (_) { setMessage({status: 'error'}); }
    finally { active.current = false; setBusy(false); }
  };
  return U.jsxs('div', {className: 'cpa-qoder-actions', style: {display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '8px'}, children: [
    U.jsx(W, {type: 'button', size: 'sm', variant: 'secondary', disabled: disabled || busy, loading: busy, onClick: () => run(async () => {
      const result = await cpaQoderCheckin(file, t);
      setMessage(result);
    }), children: t('qoder_quota.checkin')}),
    U.jsxs('label', {children: [U.jsx('input', {type: 'checkbox', checked: auto, disabled: disabled || busy, onChange: event => {
      const enabled = event.target.checked;
      return run(async () => {await cpaQoderSetAutoCheckin(file, enabled); setAuto(enabled);});
    }}), ' ', t('qoder_quota.auto_checkin')]}),
    U.jsx('span', {role: 'status', 'aria-live': 'polite', children: message === null ? cpaQoderInitialCheckinDisplay(file, t, i18n.resolvedLanguage) : cpaQoderCheckinDisplay(message, t, i18n.resolvedLanguage)})
  ]});
}
`

// AddQoderQuotaToManagementHTML runs after AddWorkBuddyQuotaToManagementHTML.
func AddQoderQuotaToManagementHTML(data []byte) ([]byte, error) {
	if bytes.Contains(data, []byte("function cpaQoderQuota(")) {
		return data, nil
	}
	icon, _ := json.Marshal(qoderIcon)
	runtime := qoderQuotaRuntime + fmt.Sprintf("TD.qoder=%s;TD['qoder-cn']=%s;", icon, icon)
	patches := []struct{ old, replacement string }{
		{"xaiQuota:{}", "xaiQuota:{},qoderQuota:{},qoderCNQuota:{}"},
		{"setXaiQuota:t=>", "setQoderQuota:t=>e(e=>({qoderQuota:Dm(t,e.qoderQuota)})),setQoderCNQuota:t=>e(e=>({qoderCNQuota:Dm(t,e.qoderCNQuota)})),setXaiQuota:t=>"},
		{"xaiQuota:i(e.xaiQuota)", "xaiQuota:i(e.xaiQuota),qoderQuota:i(e.qoderQuota),qoderCNQuota:i(e.qoderCNQuota)"},
		{"var Wk={", runtime + "var Wk={qoder:cpaQoderQuota('qoder'),\"qoder-cn\":cpaQoderQuota('qoder-cn'),"},
		{"OM=[`claude`", "OM=[`qoder`,`qoder-cn`,`claude`"},
		{"AM={workbuddy:", "AM={qoder:Wk.qoder.filterFn,\"qoder-cn\":Wk['qoder-cn'].filterFn,workbuddy:"},
		{"gD=new Set([`workbuddy`", "gD=new Set([`qoder`,`qoder-cn`,`workbuddy`"},
		{"PD=(e,t)=>{if(t==='workbuddy'", "PD=(e,t)=>{if(t==='qoder'||t==='qoder-cn')return e(t==='qoder'?'qoder_quota.site_intl':'qoder_quota.site_cn');if(t==='workbuddy'"},
		{"b||n===`devin`||n===`workbuddy`", "b||n===`qoder`||n===`qoder-cn`||n===`devin`||n===`workbuddy`"},
		{"(n===`devin`||n===`workbuddy`", "(n===`qoder`||n===`qoder-cn`||n===`devin`||n===`workbuddy`"},
		{"M=Om(e=>e.xaiQuota),cpaWB=", "M=Om(e=>e.xaiQuota),cpaQ=Om(e=>e.qoderQuota),cpaQCN=Om(e=>e.qoderCNQuota),cpaWB="},
		{"workbuddy:cpaWB,\"workbuddy-ai\":cpaWBAI}),[E,D,O,k,A,j,M,cpaWB,cpaWBAI])", "workbuddy:cpaWB,\"workbuddy-ai\":cpaWBAI,qoder:cpaQ,\"qoder-cn\":cpaQCN}),[E,D,O,k,A,j,M,cpaWB,cpaWBAI,cpaQ,cpaQCN])"},
		{"B=tO(n),[V]", "B=cpaIsQoder(n)?cpaQoderIdentity(n,t):tO(n),[V]"},
		{"d=t.file,f=Tm(d),", "d=t.file,f=cpaIsQoder(d)?cpaQoderIdentity(d,l).fullName:Tm(d),"},
		{"[e.name,e.type,e.provider,e.email,e.projectId].some", "[e.name,e.type,e.provider,e.email,e.projectId,...cpaQoderSearch(e)].some"},
		{"[e.name,e.email].some", "[e.name,e.email,...cpaQoderSearch(e)].some"},
		{"!C&&(0,U.jsxs)(`div`,{className:aA.toggleWrap", "cpaIsQoder(n)&&!C&&(0,U.jsx)(cpaQoderAccountActions,{file:n,disabled:o||D}),!C&&(0,U.jsxs)(`div`,{className:aA.toggleWrap"},
	}
	// The WorkBuddy patch makes the card selector provider-independent.
	if !bytes.Contains(data, []byte("d=Om(e=>l.storeSelector(e)[u])")) {
		return nil, fmt.Errorf("management html Qoder quota requires the WorkBuddy quota patch")
	}
	for _, patch := range patches {
		if !bytes.Contains(data, []byte(patch.old)) {
			return nil, fmt.Errorf("management html Qoder quota anchor not found: %s", patch.old)
		}
		data = bytes.ReplaceAll(data, []byte(patch.old), []byte(patch.replacement))
	}
	matches := qoderQuotaLocaleAnchor.FindAllIndex(data, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("management html Qoder quota locale anchor not found")
	}
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		sample := data[match[0]:min(len(data), match[1]+120)]
		labels := map[string]string{
			"site_intl": "Qoder International", "site_cn": "Qoder China", "plan": "Plan", "remaining": "Personal credits", "remaining_percent": "Remaining", "used": "Credits used",
			"base": "Plan credits", "addon": "Extra credits / check-in rewards", "package": "Credit package", "shared": "Shared credits", "unlimited": "Unlimited", "unavailable": "Unavailable", "organization": "Organization", "expires_at": "Expires at",
			"idle": "Click here to refresh quota", "loading": "Loading quota...", "load_failed": "Failed to load quota: {{message}}", "missing_auth_index": "Account unavailable; refresh the account list",
			"empty_title": "No Qoder accounts", "empty_desc": "Sign in to Qoder to view credits", "empty_data": "No active credit package",
			"checkin": "Check in", "auto_checkin": "Automatic check-in", "checkin_claimed": "Claimed {{credits}} credits", "checkin_already": "Already checked in today", "checkin_none": "No reward available",
			"checkin_failed": "Check-in failed", "action_failed": "Could not complete this action; try again",
			"checkin_status_claimed": "Claimed {{credits}} credits", "checkin_status_success": "Check-in completed", "checkin_status_already": "Already checked in this round", "checkin_status_skipped": "No reward available or waiting for the next attempt",
			"checkin_status_blocked": "The server blocked a claim for this round", "checkin_status_unavailable": "Check-in unavailable; verify device identity and campaign eligibility", "checkin_status_error": "Check-in failed",
			"checkin_next_attempt": "Next attempt: {{time}}", "checkin_last_success": "Last success: {{time}}",
		}
		if bytes.Contains(sample, []byte("额度")) || bytes.Contains(sample, []byte("配額")) {
			labels = map[string]string{
				"site_intl": "Qoder 国际站", "site_cn": "Qoder 国内站", "plan": "套餐", "remaining": "个人积分", "remaining_percent": "剩余比例", "used": "已用积分",
				"base": "套餐内积分", "addon": "额外积分 / 签到奖励", "package": "积分包", "shared": "共享积分", "unlimited": "无限额度", "unavailable": "暂不可用", "organization": "组织", "expires_at": "到期时间",
				"idle": "点击此处刷新额度", "loading": "正在加载额度...", "load_failed": "额度获取失败：{{message}}", "missing_auth_index": "账号暂不可用，请刷新账号列表",
				"empty_title": "暂无 Qoder 账号", "empty_desc": "登录 Qoder 后查看积分", "empty_data": "暂无有效积分套餐",
				"checkin": "签到", "auto_checkin": "自动签到", "checkin_claimed": "已领取 {{credits}} 积分", "checkin_already": "今天已签到", "checkin_none": "暂无可领取奖励",
				"checkin_failed": "签到失败", "action_failed": "操作失败，请重试",
				"checkin_status_claimed": "已领取 {{credits}} 积分", "checkin_status_success": "签到完成", "checkin_status_already": "本轮已签到", "checkin_status_skipped": "暂无奖励或等待下次尝试",
				"checkin_status_blocked": "本轮奖励领取被服务端拒绝", "checkin_status_unavailable": "签到暂不可用，请检查设备身份与活动资格", "checkin_status_error": "签到失败",
				"checkin_next_attempt": "下次尝试：{{time}}", "checkin_last_success": "最近成功：{{time}}",
			}
		}
		encoded, errMarshal := json.Marshal(labels)
		if errMarshal != nil {
			return nil, errMarshal
		}
		addition := append([]byte(`"qoder_quota":`), encoded...)
		data = insertBytes(data, match[0], append(addition, ','))
	}
	return data, nil
}
