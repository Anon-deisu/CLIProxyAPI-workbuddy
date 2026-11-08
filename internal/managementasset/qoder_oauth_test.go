package managementasset

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

func TestQoderManagementCompletePatchChain(t *testing.T) {
	data, errRead := os.ReadFile("../../static/management.html")
	if errRead != nil {
		t.Fatal(errRead)
	}
	patches := []struct {
		name  string
		apply func([]byte) ([]byte, error)
	}{
		{"Cursor OAuth", AddCursorOAuthToManagementHTML},
		{"Cursor API key", AddCursorAPIKeyInlineToManagementHTML},
		{"WorkBuddy OAuth", AddWorkBuddyOAuthToManagementHTML},
		{"WorkBuddy quota", AddWorkBuddyQuotaToManagementHTML},
		{"Qoder OAuth", AddQoderOAuthToManagementHTML},
		{"Qoder quota", AddQoderQuotaToManagementHTML},
		{"Runtime scheduler", AddRuntimeSchedulerToManagementHTML},
		{"Codex auto OAuth", AddCodexAutoOAuthToManagementHTML},
		{"Agent workspace", AddAgentWorkspaceToManagementHTML},
		{"Rolling usage", AddRollingTokenUsageToManagementHTML},
		{"Performance", AddPerformanceToManagementHTML},
	}
	for _, patch := range patches {
		var errPatch error
		data, errPatch = patch.apply(data)
		if errPatch != nil {
			t.Fatalf("%s: %v", patch.name, errPatch)
		}
	}
	for _, patch := range patches {
		again, errPatch := patch.apply(data)
		if errPatch != nil || !bytes.Equal(data, again) {
			t.Fatalf("%s is not idempotent: %v", patch.name, errPatch)
		}
	}
	node, errNode := exec.LookPath("node")
	if errNode != nil {
		t.Skip("Node.js is required for the compiled management bundle test")
	}
	command := exec.Command(node, "--experimental-vm-modules", "-e", qoderManagementVMTest)
	command.Stdin = bytes.NewReader(data)
	if output, errRun := command.CombinedOutput(); errRun != nil {
		t.Fatalf("management patch chain VM validation: %v\n%s", errRun, output)
	}
}

func TestQoderManagementRejectsMissingAnchors(t *testing.T) {
	for _, patch := range []func([]byte) ([]byte, error){AddQoderOAuthToManagementHTML, AddQoderQuotaToManagementHTML} {
		if data, errPatch := patch([]byte("<html><body>unknown bundle</body></html>")); errPatch == nil || data != nil {
			t.Fatal("unsupported management bundle was partially patched")
		}
	}
}

// Execute the actual patched native store, registry, card and page refresh paths.
// Framework primitives, unrelated providers and the HTTP client use deterministic fakes.
const qoderManagementVMTest = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const html = require('node:fs').readFileSync(0, 'utf8');
const scripts = [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)];
assert(scripts.length > 1, 'the complete served patch chain was not exercised');
for (const [_, attrs, code] of scripts) {
  if (!code.trim()) continue;
  if (/type=["']module["']/.test(attrs)) new vm.SourceTextModule(code);
  else new vm.Script(code);
}
const source = scripts.map(match => match[2]).sort((a,b) => b.length-a.length)[0];
function between(start, end) {
  const from = source.indexOf(start);
  assert(from >= 0, 'missing native start: ' + start);
  const to = source.indexOf(end, from + start.length);
  assert(to > from, 'missing native end: ' + end);
  return source.slice(from, to);
}
const localeObjects = [...source.matchAll(/"qoder_quota":(\{.*?\}),/g)].map(match => JSON.parse(match[1]));
const en = localeObjects.find(value => value.site_cn === 'Qoder China');
const zh = localeObjects.find(value => value.site_cn === 'Qoder 国内站');
assert(en && zh, 'both site locales must be installed');
let language = 'en';
const translate = (key, values = {}) => {
  const labels = language === 'zh' ? zh : en;
  let result = key.startsWith('qoder_quota.') ? labels[key.slice(12)] || key : key;
  for (const [name, value] of Object.entries(values)) result = result.replaceAll('{{' + name + '}}', String(value));
  return result;
};
let hooks = [], cursor = 0;
const y = {
  useState(value) { const index = cursor++; if (!(index in hooks)) hooks[index] = typeof value === 'function' ? value() : value; return [hooks[index], value => {hooks[index] = typeof value === 'function' ? value(hooks[index]) : value;}]; },
  useRef(value) {const index = cursor++; return hooks[index] ??= {current:value};},
  useMemo: callback => callback(), useCallback: callback => callback, useEffect() {}
};
const U = {Fragment: 'fragment', jsx: (type, props, key) => ({type, props, key}), jsxs: (type, props, key) => ({type, props, key})};
function render(callback, props, reset = true) {if (reset) hooks = []; cursor = 0; return callback(props);}
function nodes(tree) {
  if (tree == null || typeof tree === 'boolean') return [];
  if (Array.isArray(tree)) return tree.flatMap(nodes);
  if (typeof tree !== 'object') return [tree];
  return [tree, ...nodes(tree.props?.children)];
}
function text(tree) {return nodes(tree).filter(value => typeof value === 'string' || typeof value === 'number').join(' ');}
function Cc(init) {
  let state;
  const set = update => {state = {...state, ...(typeof update === 'function' ? update(state) : update)};};
  state = init(set, () => state);
  const store = selector => selector(state);
  store.getState = () => state;
  return store;
}
let requests = [], pending = [], checkins = [], fields = [];
const payload = {plan:'Pro', realm:'intl', credits:{total:120,used:45.5,remaining:74.5,remaining_percent:62.0833}, packages:[
  {name:'Base credits',source:'userQuota',total:100,used:40.5,remaining:59.5,remaining_percent:59.5},
  {name:'Add-on credits',source:'addOnQuota',total:20,used:5,remaining:15,remaining_percent:75}
], shared:{total:-1,cap:-1,used:0,remaining:999999,available:true,unlimited:true,organization_name:'Example team'}};
const sp = {
  get(path, options) {requests.push({path,options}); return new Promise((resolve,reject) => pending.push({resolve,reject}));},
  async post(path, body, options) {checkins.push({path,body,options}); return {status:'success',claimed:1,already:1,credits:10};},
  async patch(path, body) {fields.push({path,body}); return {status:'ok'};}
};
const context = vm.createContext({assert, Cc, U, y, sp, Intl, Date, Error, console,
  H: () => ({t:translate,i18n:{resolvedLanguage:language}}),
  Kc: selector => selector({showNotification(){},showConfirmation(){}}),
  TD:{}, DD: value => value, qk:{}, Jk:{}, VM:{}, HM:{}, W:'button', Cs:'refresh',
  M_: error => error.status, Gg: (t,status,message) => message, RD: () => false,
  aA:{}, FD: () => ({}), OD: () => false, AD: () => '', MD: () => false, XD: () => '-', mm: () => ({})
});
const run = code => vm.runInContext(code, context);
for (const [name,type] of Object.entries({fO:'antigravity',RO:'claude',hk:'codex',ik:'devin',Dk:'kimi',Tk:'meta',Fk:'xai'})) {
  const key = type === 'antigravity' ? 'antigravityQuota' : type + 'Quota';
  const setter = 'set' + type[0].toUpperCase() + type.slice(1) + 'Quota';
  context[name] = {type,filterFn:file => (file.provider || file.type) === type,storeSelector:state => state[key],storeSetter:setter,
    buildLoadingState:() => ({status:'loading'}),buildSuccessState:data => ({...data,status:'success'}),buildErrorState:(error,errorStatus) => ({status:'error',error,errorStatus})};
}
for (const name of ['MO','KO','yk','ak','Ok','Ek','Uk']) context[name] = name;
for (const name of ['nS','hS','mS','tS','eS','pD','fS','pS','rS','iS']) context[name] = name;
for (const name of ['_c','uw','rA','pw','hs','ws','ys','Es']) context[name] = name;
run(between('function lm(e)', 'function um(e)'));
run(between('function hm(e)', ',jm=null') + ';');
run(between('function YD(e)', 'var XD='));
run(between('var $g=', 'function e_(e)'));
run('var ' + between('gD=new Set([', ',_D=') + ';');
run('var ' + between('PD=(e,t)=>{', ',FD=') + ';');
run('var ' + between('ID=(e,t)=>{', ',LD=') + ';');
run(between('function pO(', 'function mO('));
run(between('function cpaWorkBuddyQuota(', ',qk=') + ';');
run(between('var OM=[', 'var BM='));
run(between('function Xk(e)', 'var Zk='));
run(between('var $D=', ',nO=') + ';');
run('var ' + between('iO=(e,t)=>', ',aO=') + ';');
run(between('function oA(e)', 'function sA('));
run(between('function UM(e)', 'var WM='));
run(between('var dN=', 'var mN='));
run(between('var lM=[', 'function _M('));
run('var Ry={' + between('patchFields:(e,t)=>', ',requestManualRefresh:') + '};');
for (const match of source.matchAll(/JSON\.parse\(\x60[\s\S]*?\x60\)/g)) {
  // Serialized locales must still be valid JSON after inserting translated keys.
  if (match[0].includes('qoder_oauth_title')) vm.runInNewContext(match[0]);
}
assert(context.uM.has('qoder') && context.uM.has('qoder-cn'));
assert(!context.dM.has('qoder') && !context.dM.has('qoder-cn'), 'device flows must not show callback controls');
assert.equal(context.hM('qoder-cn','oauth_button'), 'auth_login.qoder_cn_oauth_button');
const oauthTitles = [...source.matchAll(/"qoder(?:_cn)?_oauth_title":"([^"]*)"/g)].map(match=>match[1]);
for (const title of ['Qoder International','Qoder China','Qoder 国际站','Qoder 国内站']) assert(oauthTitles.includes(title),'missing OAuth card title: '+title);
for (const provider of ['qoder','qoder-cn','workbuddy','workbuddy-ai','claude','antigravity','codex','xai','kimi','devin','meta']) {
  assert(context.Wk[provider], 'quota adapter missing: ' + provider);
  assert(context.OM.includes(provider), 'quota tab missing: ' + provider);
}
for (const provider of ['qoder','qoder-cn','workbuddy','workbuddy-ai']) assert.equal(typeof context.Wk[provider].storeSelector,'function',provider+' store selector is not callable');
const pickerStart = source.indexOf('(0,U.jsxs)(\x60div\x60,{className:\x60cpa-quota-provider-picker\x60');
assert(pickerStart >= 0, 'quota provider picker must separate the fixed All button');
const pickerEnd = source.indexOf(']})', pickerStart) + 3;
context.VA = 'native-provider-tabs';
context.SN = ['all', ...context.OM];
context.R = Object.fromEntries(context.SN.map((type, index) => [type,index+1]));
context.l = 'workbuddy'; context.n = 'dark'; context.ie = type => {context.l=type;};
const picker = run(source.slice(pickerStart, pickerEnd));
const [allTabs, providerTabs] = picker.props.children;
assert.deepEqual(Array.from(allTabs.props.types), ['all']);
assert.deepEqual(Array.from(providerTabs.props.types), Array.from(context.OM));
assert.equal(providerTabs.props.active, 'workbuddy');
assert.equal(providerTabs.props.counts.workbuddy, context.R.workbuddy);
providerTabs.props.onChange('workbuddy-ai');
assert.equal(context.l, 'workbuddy-ai', 'provider selection must retain the native change handler');
allTabs.props.onChange('all');
assert.equal(context.l, 'all', 'fixed All must still reset the native filter');
assert.equal(context.Wk.claude.Body, 'KO');
for (const [provider, site] of [['qoder','Qoder International'],['qoder-cn','Qoder China'],['workbuddy','workbuddy_quota.site_cn'],['workbuddy-ai','workbuddy_quota.site_ai']]) {
  assert.equal(context.PD(translate,provider), site);
  assert(context.ID(provider,'light').startsWith('data:image/svg+xml'));
}
const intl = {name:'qoder-hash.json',type:'qoder',nickname:'Ada',uid:'9007199254740993',auth_index:'idx-intl',realm:'intl'};
const cn = {name:'qoder-cn-hash.json',provider:'qoder-cn',nickname:'博雅',uid:'uid-cn',authIndex:'idx-cn',realm:'cn'};
assert.equal(context.Wk.qoder.filterFn(intl), true);
assert.equal(context.Wk.qoder.filterFn(cn), false);
assert.equal(context.Wk.qoder.filterFn({...intl,disabled:true}), false);
assert.equal(context.Wk.qoder.filterFn({...intl,runtime_only:true}), false);
assert.equal(context.cpaQoderIdentity(intl,translate).primary,'Ada');
assert.equal(context.cpaQoderIdentity({...intl,nickname:'',label:'Account label'},translate).primary,'Account label');
assert.equal(context.cpaQoderIdentity({...intl,nickname:'',label:''},translate).primary,intl.uid);
assert.equal(context.NM([intl,cn]).length,2);
assert.equal(context.FM(context.NM([intl,cn]),'博雅')[0].file,cn);
assert.equal(context.FM(context.NM([intl,cn]),intl.uid)[0].file,intl);
function card(file) {return render(context.Xk,{file,quotaType:file.provider || file.type,disableControls:false});}
function page(file) {return render(context.UM,{entry:{type:file.provider || file.type,file},quota:context.dN(context.Wk[file.provider || file.type],file),resolvedTheme:'light',canRefresh:true,onRefresh(){}});}
function account(file) {return render(context.oA,{file,statusUpdating:{},manualRefreshing:{},cooldownResetting:{},statusBarCache:new Map,quotaFilterType:'all'});}
function cardRefresh(file) {
  const tree = card(file);
  const button = nodes(tree).find(node => node.props?.onClick && node.props.children === translate('qoder_quota.idle')) || nodes(tree).find(node => node.props?.onClick && text(node).includes('auth_files.quota_refresh_single'));
  assert(button, 'native auth card refresh button missing');
  return button.props.onClick();
}
const tick = async () => {for(let i=0;i<8;i++) await Promise.resolve();};
(async () => {
  assert.match(text(page(intl)),/Ada.*UID: 9007199254740993.*Qoder International/);
  assert.match(text(account(intl)),/Ada.*UID: 9007199254740993.*Qoder International/);
  assert(nodes(account(intl)).some(node=>node.type===context.cpaQoderAccountActions),'Qoder account actions are not mounted');
  language='zh';
  assert.match(text(page(cn)),/博雅.*UID: uid-cn.*Qoder 国内站/);
  assert.match(text(account(cn)),/博雅.*UID: uid-cn.*Qoder 国内站/);
  assert.equal(context.PD(translate,'qoder'),'Qoder 国际站');
  language='en';
  const unrelated = {name:'original.json',type:'claude',nickname:'ignored'};
  assert.match(text(page(unrelated)),/original.json/);
  assert(!text(page(unrelated)).includes('ignored'));
  assert(!nodes(account(unrelated)).some(node=>node.type===context.cpaQoderAccountActions));
  assert(!text(account(unrelated)).includes('ignored'));
  cardRefresh(intl);
  assert.equal(context.dN(context.Wk.qoder,intl).status,'loading');
  assert.match(text(page(intl)),/Loading quota/);
  assert.equal(requests[0].path,'/credentials/qoder-quota');
  assert.equal(requests[0].options.params.auth_index,'idx-intl');
  const refreshPage = render(context.fN,false).refreshQuota;
  await refreshPage(intl,context.Wk.qoder);
  assert.equal(requests.length,1,'page refresh must share the card loading state');
  pending.shift().resolve(payload); await tick();
  assert.equal(context.dN(context.Wk.qoder,intl).credits.remaining,74.5);
  const component = nodes(card(intl)).find(node => node.type === context.cpaQoderQuotaBody);
  assert(component, 'card is not rendering the registered Qoder body');
  const body = context.cpaQoderQuotaBody(component.props);
  assert.match(text(body),/74.5 \/ 120/);
  assert.match(text(body),/Extra credits \/ check-in rewards/);
  assert.match(text(body),/Unlimited/);
  assert(!text(body).includes('999999') && !text(body).includes('-1'));
  assert.match(text(context.cpaQoderQuotaBody({quota:{shared:{...payload.shared,available:false}},classes:{}})),/Unavailable/);
  language='zh';
  assert.match(text(context.cpaQoderQuotaBody(component.props)),/额外积分 \/ 签到奖励/);
  language='en';
  const next = refreshPage(cn,context.Wk['qoder-cn']);
  assert.equal(requests.at(-1).options.params.auth_index,'idx-cn');
  pending.shift().reject(Object.assign(vm.runInContext("new Error('temporarily unavailable')", context),{status:503})); await next;
  assert.equal(context.dN(context.Wk['qoder-cn'],cn).status,'error');
  assert.match(text(card(cn)),/temporarily unavailable/);
  assert.match(text(page(cn)),/temporarily unavailable/);
  const retry = refreshPage(cn,context.Wk['qoder-cn']);
  context.Om.getState().clearQuotaCache([cn.name]);
  pending.shift().resolve({...payload,realm:'cn'}); await retry;
  assert.equal(context.dN(context.Wk['qoder-cn'],cn),undefined,'deleted account was repopulated by a late quota result');
  assert.equal(context.dN(context.Wk.qoder,intl).status,'success','deleting CN cleared international quota');
  const batch = render(context.pN).loadQuota(context.NM([intl,cn]));
  for (const promise of pending.splice(0)) promise.resolve(payload);
  await batch;
  const selection = between('let E=Om(e=>e.antigravityQuota)', ',F=WO(');
  const map = run('(()=>{' + selection + ';return N;})()');
  assert.equal(map.qoder[intl.name].status,'success');
  assert.equal(map['qoder-cn'][cn.name].status,'success');
  context.Om.getState().clearQuotaCache();
  for (const provider of ['qoder','qoder-cn','workbuddy','workbuddy-ai']) assert.equal(Object.keys(context.Kk(context.Wk[provider])).length,0,'global quota reset omitted '+provider);
  assert.equal(context.cpaQoderAutoCheckin(intl),false);
  assert.equal(context.cpaQoderAutoCheckin({...intl,qoder_auto_checkin:'true'}),false);
  assert.equal(context.cpaQoderAutoCheckin({...intl,metadata:{qoder_auto_checkin:true}}),true);
  assert(context.cpaQoderSearch({...intl,organization_name:'A team',organization_id:'org-7'}).includes('A team'));
  assert(context.cpaQoderSearch({...intl,organization_name:'A team',organization_id:'org-7'}).includes('org-7'));
  assert.equal(context.cpaQoderCheckinStatusText({status:'success',claimed:1,already:2,credits:12.5},translate),'Claimed 12.5 credits');
  assert.match(context.cpaQoderCheckinStatusText({status:'blocked',claimed:1,credits:10},translate),/Claimed 10 credits.*blocked/);
  assert.match(context.cpaQoderCheckinStatusText({status:'unavailable'},translate),/device identity.*eligibility/);
  assert.equal(context.cpaQoderCheckinStatusText({status:'already',already:1},translate),'Already checked in this round');
  assert.equal(context.cpaQoderCheckinStatusText({status:'error'},translate),'Check-in failed');
  language='zh';
  assert.match(context.cpaQoderCheckinStatusText({status:'unavailable'},translate),/设备身份.*活动资格/);
  assert.match(context.cpaQoderCheckinStatusText({status:'blocked'},translate),/服务端拒绝/);
  language='en';
  const savedActions = render(context.cpaQoderAccountActions,{file:{...intl,qoder_auto_checkin:true,qoder_checkin_last_result:{status:'unavailable'},qoder_checkin_next_attempt:1893456000},disabled:false});
  assert(nodes(savedActions).find(node=>node.type==='input').props.checked);
  assert.match(text(savedActions),/Check-in unavailable.*Next attempt/);
  const actions = render(context.cpaQoderAccountActions,{file:intl,disabled:false});
  const checkbox = nodes(actions).find(node=>node.type==='input');
  assert.equal(checkbox.props.checked,false);
  assert.equal(checkins.length,0,'rendering must never check in automatically');
  await checkbox.props.onChange({target:{checked:true}});
  assert.equal(fields[0].path,'/credentials/fields');
  assert.equal(fields[0].body.name,intl.name);
  assert.equal(fields[0].body.qoder_auto_checkin,true);
  const button = nodes(actions).find(node=>node.type==='button');
  await button.props.onClick();
  assert.equal(checkins[0].path,'/credentials/qoder-checkin');
  assert.equal(checkins[0].options.params.auth_index,'idx-intl');
  assert.equal(context.dN(context.Wk.qoder,intl),undefined,'check-in must invalidate both quota views');
  await assert.rejects(context.Wk.qoder.fetchQuota({type:'qoder'},translate), /Account unavailable/);
  console.log('Full patch chain parsed; native registries, titles, stores, refresh, deletion, quota rendering and check-in contracts passed.');
})().catch(error => {console.error(error);process.exitCode=1;});
`
