'use strict';
'require view';
'require rpc';
'require ui';
'require poll';

function call(name, params) {
 var fn=rpc.declare({object:'fsd',method:name,params:params||[],reject:true});
 return function() {return fn.apply(null,arguments).then(function(r){if(!r||r.error)throw new Error(r&&r.error||_('日志服务不可用'));return r;});};
}
var read=call('logging'), save=call('save_logging',['draft']), test=call('test_logging',['draft']);
function mib(n){return ((n||0)/1048576).toFixed(2)+' MiB';}
return view.extend({
 handleSave:null,handleSaveApply:null,handleReset:null,
 load:function(){return read();},
 render:function(initial){
  var writable=L.hasViewPermission()&&window.location.protocol==='https:';
  var busy=false, cfg=initial.config||{};
  var keys=E('input',{type:'checkbox'});keys.checked=!!cfg.tls_keylog;
  var csv=E('input',{type:'checkbox'}),nas=E('input',{type:'checkbox'});
  csv.checked=!!cfg.local_csv;nas.checked=!!cfg.enabled;
  function input(value,type){return E('input',{'class':'cbi-input-text',type:type||'text',value:value||'',autocomplete:'off',spellcheck:'false'});}
  var endpoint=input(cfg.endpoint),device=input(cfg.device_id),token=input('','password'),ca=input(cfg.ca_file);
  token.setAttribute('autocomplete','new-password');token.setAttribute('placeholder',_('留空保留已有令牌'));
  var controls=[keys,csv,nas,endpoint,device,token,ca];controls.forEach(function(n){n.disabled=!writable;});
  var fieldID=0;
  function field(label,node,hint){var id='fsd-log-'+(++fieldID);node.setAttribute('id',id);return E('div',{'class':'cbi-value'},[E('label',{'class':'cbi-value-title','for':id},label),E('div',{'class':'cbi-value-field'},[node,E('div',{'class':'cbi-value-description'},hint||'')])]);}
  var status=E('div',{'aria-live':'polite'}),message=E('p',{'role':'status'}),secretState=E('p');
  function show(state){
   var u=state.upload||{},c=state.csv||{},k=state.tls_keylog||{};
   secretState.textContent=state.token_present?_('已保存上传令牌；不会回显。'):_('尚未配置上传令牌。');
   var rows=[[_('本地 CSV'),c.enabled?_('已开启'):_('已关闭')],[_('CSV 已保存 / 失败'),(c.saved||0)+' / '+(c.failed||0)],[_('NAS 上传'),u.enabled?(u.inflight?_('正在上传'):_('已开启')):_('已关闭')],[_('已上传 / 待上传'),(u.uploaded||0)+' / '+((u.queued||0)+(u.inflight?1:0))],[_('内存缓存'),mib(u.buffer_bytes)+' / '+mib(u.buffer_limit||67108864)],[_('丢失 / 重试'),(u.dropped||0)+' / '+(u.retries||0)],[_('最近成功时间（设备时区）'),u.last_upload||'—'],[_('最近上传错误'),u.last_error||'—'],[_('最近 CSV 错误'),c.last_error||'—']];
   rows.push([_('TLS 密钥记录'),k.enabled?_('已开启'):_('已关闭')],[_('密钥记录 / 丢失条数'),(k.saved||0)+' / '+(k.dropped||0)],[_('密钥文件已上传 / 重试次数'),(k.uploaded||0)+' / '+(k.retries||0)],[_('密钥内存缓存'),mib(k.buffer_bytes)+' / 8 MiB'],[_('最近密钥日志错误'),k.last_error||'—']);
   status.replaceChildren(E('table',{'class':'table'},rows.map(function(r){return E('tr',{'class':'tr'},[E('th',{'class':'th'},r[0]),E('td',{'class':'td'},String(r[1]))]);})));
  }
  function draft(){return JSON.stringify({tls_keylog:keys.checked,local_csv:csv.checked,enabled:nas.checked,endpoint:endpoint.value.trim(),device_id:device.value.trim(),ca_file:ca.value.trim(),token:token.value});}
  function action(label,fn){var b=E('button',{'class':'cbi-button cbi-button-action',type:'button'},label);b.disabled=!writable;b.addEventListener('click',async function(){if(busy)return;busy=true;buttons.forEach(function(n){n.disabled=true;});controls.forEach(function(n){n.disabled=true;});try{await fn();}catch(e){message.textContent=e.message;ui.addNotification(null,E('p',{},e.message),'error');}finally{busy=false;buttons.forEach(function(n){n.disabled=!writable;});controls.forEach(function(n){n.disabled=!writable;});}});return b;}
  var buttons=[action(_('保存并应用'),async function(){var state=await save(draft());token.value='';show(state);message.textContent=_('日志配置已生效，未重启 FSD。');}),action(_('测试 HTTPS 与令牌'),async function(){var result=await test(draft());message.textContent=result.message;})];
  show(initial);
  poll.add(function(){if(busy)return Promise.resolve();return read().then(show).catch(function(e){message.textContent=e.message;});},3);
  return E('div',{'class':'cbi-map'},[
   E('h2',{},_('FSD 日志记录')),
   E('p',{},_('日志设置独立于业务 YAML。各个开关互不影响，可同时开启或全部关闭。')),
   !writable?E('p',{},_('请使用 HTTPS 登录并取得写权限后修改。')):E('span'),
   E('h3',{},_('本地 CSV')),
   field(_('启用本地 CSV'),csv,_('记录命中的 HTTPS JSON 请求；目录 /etc/fsd/request-logs，总量 100 MiB，满时删除最旧文件。关闭不删除已有文件。CSV 不记录 TLS 握手失败。')),
   E('h3',{},_('NAS 日志上传')),
   field(_('启用 NAS 上传'),nas,_('记录 TLS 握手失败、请求不完整和请求已接收三种事件，经 HTTPS 接收服务写入数据库。关闭不会启用本地 CSV。')),
   field(_('HTTPS 接收地址'),endpoint,_('完整地址，例如 https://logs.example.com/v1/events')),
   field(_('设备标识'),device,_('必须与 NAS 接收服务允许的设备标识一致。')),
   field(_('上传令牌'),token,_('填写新令牌以替换；留空保留。至少 32 个字符。')),secretState,
   field(_('CA 证书路径（可选）'),ca,_('设备上的绝对路径。留空使用系统可信 CA；不会跳过证书验证。')),
   E('p',{},_('NAS 缓存仅存内存，上限 64 MiB。缓存不足时继续服务并统计日志丢失，不截断请求体。重启会丢失未上传缓存；修改 NAS 设置前须等待缓存清空。')),
   E('h3',{},_('TLS 会话密钥（调试）')),
   field(_('启用 TLS 密钥记录'),keys,_('仅记录目标设备与 FSD 业务端口建立的 TLS 会话。启用后请重新建立连接；仅监控模式不记录。用于 Wireshark 解密，请限制 NAS 文件访问权限。')),
   E('p',{},_('内存目录 /tmp/fsd/tls-keylogs，每分钟或 1 MiB 轮转，经独立抓包服务的 SFTP 通道上传到 /tls-keylogs。缓存上限 8 MiB，满时丢弃新密钥记录并继续服务。关闭后仍上传已有缓存，设备重启会丢失未上传内容。NAS 必须配置该目录映射。')),
   E('div',{'class':'cbi-page-actions'},buttons),message,
   E('h3',{},_('运行状态')),E('p',{},_('每 3 秒刷新。计数自本次日志组件启动起累计，修改 NAS 设置可能重置上传计数。连接测试仅验证 HTTPS 和令牌，不写入测试记录，也不验证数据库写入。')),status
  ]);
 }
});
