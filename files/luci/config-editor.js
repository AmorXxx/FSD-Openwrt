'use strict';
'require baseclass';

function action(label, callback) {
	var node = E('button', { 'type': 'button', 'class': 'cbi-button cbi-button-action' }, label);
	node.addEventListener('click', callback);
	return node;
}

function input(className, value, placeholder, multiline) {
	var node = E(multiline ? 'textarea' : 'input', {
		'class': className + (multiline ? ' cbi-input-textarea' : ' cbi-input-text'),
		'placeholder': placeholder,
		'autocapitalize': 'off',
		'autocorrect': 'off',
		'spellcheck': 'false'
	});
	node.value = value;
	if (multiline)
		node.rows = 2;
	else
		node.type = 'text';
	return node;
}

function field(label, node, className) {
	return E('label', { 'class': 'fsd-form-field ' + (className || '') }, [E('span', {}, label), node]);
}

function jsonValue(node) {
	var raw = node.value.trim();
	try {
		JSON.parse(raw);
	} catch (error) {
		node.closest('details').open = true;
		node.focus();
		throw new Error(_('请输入有效的 JSON 值。字符串需要使用双引号。'));
	}
	return raw;
}

return baseclass.extend({
	create: function(initial, canEdit, save) {
		var current = initial;
		var dirty = false;
		var originalFields = new WeakMap();
		var originalRoutes = new WeakMap();
 var originalSites = new WeakMap();
		var changedRoutes = new WeakSet();
		var scopeMode = E('select', { 'class': 'cbi-input-select' }, [
			E('option', { 'value': 'all' }, _('全部设备')),
			E('option', { 'value': 'selected' }, _('指定设备'))
		]);
		var scopeAddresses = input('fsd-scope-addresses', '', '192.0.2.100\nfd00::100', true);
		scopeAddresses.rows = 4;
		var dnsList = E('div', { 'class': 'fsd-form-list' });
		var siteList = E('div', { 'class': 'fsd-form-list' });
		var feedback = E('div', { 'class': 'fsd-editor-feedback', 'role': 'status' });
		var saveButton = action(_('保存文件'), async function() {
			try {
				var draft = readDraft();
				var encoded = JSON.stringify(draft);
				if (new Blob([JSON.stringify({ draft: encoded })]).size > 60 * 1024)
					throw new Error(_('本次修改的数据过多，请分次保存大型 JSON 接口。'));
				reset(await save(encoded));
				feedback.textContent = _('文件已保存，运行配置未改变；点击「应用此文件」生效。');
			} catch (error) {
				feedback.textContent = error.message;
			}
		});
		var discardButton = action(_('放弃表单草稿'), function() { reset(current); });

		function changed() {
			dirty = true;
			saveButton.disabled = discardButton.disabled = false;
			feedback.textContent = _('有未保存的修改');
		}

		function remove(label, row, focusNext) {
			var button = action(label, function() {
				var sibling = row.nextElementSibling || row.previousElementSibling;
				var card = row.parentElement.closest('.fsd-route-card');
				row.remove();
				var target = sibling && sibling.querySelector('summary, button') || focusNext;
				if (card)
					changedRoutes.add(card);
				target.focus();
				changed();
			});
			button.classList.replace('cbi-button-action', 'fsd-remove');
			return button;
		}

		function add(list, row, limit, message) {
			if (list.children.length >= limit) {
				feedback.textContent = message;
				return;
			}
			list.appendChild(row);
			var card = list.closest('.fsd-route-card');
			if (card)
				changedRoutes.add(card);
			if (row.tagName === 'DETAILS')
				row.open = true;
			row.querySelector('input').focus();
			changed();
		}

		var addDNS = action(_('添加 DNS 规则'), function() {
			add(dnsList, dnsRow({ name: '', action: 'answer', ip: 'ap' }), 1024, _('最多可添加 1024 条 DNS 规则'));
		});
		var addSite = action(_('添加 HTTPS 站点'), function() { add(siteList, siteRow({domain:'',cert:'',key:''}),16,_('最多可添加 16 个 HTTPS 站点')); });

        function siteRow(site) {
            var row=E('div',{'class':'fsd-site-card'});
            originalSites.set(row,site.domain||'');
            var routes=E('div',{'class':'fsd-form-list fsd-site-routes'});
            var addRoute=action(_('添加此站点接口'),function(){
                add(routes,routeCard({path:'',fields:[]},addRoute),8,_('每个站点最多 8 个接口'));
            });
            routes.append(...(site.h2_routes||[]).map(function(route){return routeCard(route,addRoute);}));
            row.append(field(_('HTTPS 域名（支持 *.example.com）'),input('fsd-site-domain',site.domain,'*.example.com')),
                field(_('证书链文件路径'),input('fsd-site-cert',site.cert||'','/etc/fsd/certs/fullchain.pem')),
                field(_('私钥文件路径'),input('fsd-site-key',site.key||'','/etc/fsd/certs/privkey.pem')),
                E('div',{'class':'fsd-form-heading'},[E('h4',{},_('此站点的 HTTPS JSON 接口')),addRoute]),
                E('p',{},_('接口仅对该站点生效。未匹配路径返回 404；空列表不响应任何 JSON 接口。精确站点未命中接口时不会回退到通配站点。')),
                routes,remove(_('移除站点及其接口'),row,addSite));
            return row;
        }

		function dnsRow(record) {
 var row = E('div', { 'class': 'fsd-dns-row' });
 var mode = E('select', { 'class': 'cbi-input-select fsd-dns-action' }, [
 E('option', {value:'answer'}, _('指定 IP')), E('option', {value:'forward'}, _('上游解析')), E('option', {value:'block'}, _('阻断')) ]);
 mode.value = record.action || 'answer';
 var address=input('fsd-dns-ip', record.ip || 'ap', 'ap');
 var addressField=field(_('IPv4 或 ap'),address);
 function syncAction(){addressField.style.display=mode.value==='answer'?'':'none';address.disabled=mode.value!=='answer';}
 mode.addEventListener('change',function(){syncAction();changed();});syncAction();
 row.append(field(_('域名（支持 *.example.com）'),input('fsd-dns-name',record.name,'*.example.com'),'fsd-dns-domain'),field(_('动作'),mode),addressField,remove(_('移除规则'),row,addDNS));
 return row;
 }

		function jsonRow(entry, addField) {
			var row = E('div', { 'class': 'fsd-json-row' });
			var value;
			if (entry.value === 'true' || entry.value === 'false') {
				var toggle = E('input', { 'type': 'checkbox', 'class': 'fsd-json-boolean', 'aria-label': _('布尔值') });
				toggle.checked = entry.value === 'true';
				var state = E('span', {}, entry.value);
				toggle.addEventListener('change', function() { state.textContent = String(toggle.checked); });
				value = E('label', { 'class': 'fsd-toggle' }, [toggle, state]);
			} else {
				value = field(_('JSON 值'), input('fsd-json-value', entry.value, '"text"', true), 'fsd-json-text');
			}
			row.append(
				field(_('字段名'), input('fsd-json-name', entry.name, 'enabled'), 'fsd-json-key'),
				value,
				remove(_('移除字段'), row, addField)
			);
			originalFields.set(row, entry);
			return row;
		}

		function routeCard(route, addRoute) {
			var card = E('details', { 'class': 'fsd-route-card' });
			card.open = route.path === '/feature_configuration';
			if (route.path)
				originalRoutes.set(card, route.path);
			var summary = E('summary', {}, route.path || _('新接口'));
			var path = input('fsd-route-path', route.path, '/api/demo');
			path.addEventListener('input', function() { summary.textContent = path.value || _('新接口'); });
			var body = E('div', { 'class': 'fsd-route-body' }, field(_('接口路径（单独 * 匹配全部路径）'), path),
				E('p', {}, _('单独 * 匹配 / 和任意层级路径；/b/*/a 中的 * 仍只匹配一段。优先级：精确路径 > 单段通配规则 > 单独 *。区分大小写和末尾斜杠；单段通配符较少者优先，同等规则按配置顺序匹配。')));
            var responseMode = E('select', { 'class': 'fsd-route-response-mode' }, [
                E('option', { value: '' }, _('正常返回 JSON')),
                E('option', { value: 'close' }, _('记录后终止请求'))
            ]);
            responseMode.value = route.response_mode || '';
            body.append(field(_('响应模式'), responseMode), E('p', {}, _('终止请求模式不返回 HTTP 应答，响应 JSON 不生效。HTTP/2 仅重置当前流，保留连接及其他请求；HTTP/1.1 关闭连接。客户端可能报错或重试。记录失败仍终止请求，并写入错误日志。')));
			if (Array.isArray(route.fields)) {
				var fields = E('div', { 'class': 'fsd-form-list fsd-json-fields' });
				var addBoolean = action(_('添加布尔字段'), function() {
					add(fields, jsonRow({ name: '', value: 'false' }, addBoolean), 80, _('此接口已达到表单的 80 个字段上限'));
				});
				var addValue = action(_('添加其他字段'), function() {
					add(fields, jsonRow({ name: '', value: 'null' }, addValue), 80, _('此接口已达到表单的 80 个字段上限'));
				});
				fields.append(...route.fields.map(function(entry) { return jsonRow(entry, addBoolean); }));
				body.append(fields, E('div', { 'class': 'fsd-actions fsd-form-actions' }, [addBoolean, addValue]));
			} else {
				body.append(field(_('响应 JSON'), input('fsd-json-raw', route.json, '', true)));
			}
			body.append(E('div', { 'class': 'fsd-actions' }, remove(_('移除接口'), card, addRoute)));
			card.append(summary, body);
			return card;
		}

		function readRoute(card) {
			var path = card.querySelector('.fsd-route-path').value.trim();
			var originalPath = originalRoutes.get(card);
            var mode = card.querySelector('.fsd-route-response-mode').value;
			if (originalPath && !changedRoutes.has(card))
				return { path: path, source: originalPath, response_mode: mode };
			var raw = card.querySelector('.fsd-json-raw');
			if (raw)
				return { path: path, json: jsonValue(raw), response_mode: mode };
			var seen = new Set();
			var parts = Array.from(card.querySelectorAll('.fsd-json-row'), function(row) {
				var name = row.querySelector('.fsd-json-name');
				if (seen.has(name.value)) {
					card.open = true;
					name.focus();
					throw new Error(_('接口 %s 有重复的 JSON 字段名').format(path));
				}
				seen.add(name.value);
				var original = originalFields.get(row);
				var key = original.name === name.value && original.key || JSON.stringify(name.value);
				var toggle = row.querySelector('.fsd-json-boolean');
				var value = toggle ? String(toggle.checked) : jsonValue(row.querySelector('.fsd-json-value'));
				return key + ':' + value;
			});
			return { path: path, json: '{' + parts.join(',') + '}', response_mode: mode };
		}

		function readDraft() {
			var draft = {
				revision: current.revision,
				device_scope: { mode: scopeMode.value, addresses: scopeAddresses.value.split(/\r?\n/).map(function(v) { return v.trim(); }).filter(Boolean) },
				dns_rules: Array.from(dnsList.children, function(row) {
					return {
						name: row.querySelector('.fsd-dns-name').value.trim(),
 action: row.querySelector('.fsd-dns-action').value,
 ip: row.querySelector('.fsd-dns-action').value === 'answer' ? row.querySelector('.fsd-dns-ip').value.trim() : ''
					};
				}),
				h2_sites: Array.from(siteList.children,function(row){return {
 domain:row.querySelector('.fsd-site-domain').value.trim(),
 cert:row.querySelector('.fsd-site-cert').value.trim(),
 key:row.querySelector('.fsd-site-key').value.trim(),
 source:originalSites.get(row)||'',
 h2_routes:Array.from(row.querySelector('.fsd-site-routes').children,readRoute)
 };})
			};
            function normalized(name) { return name.trim().toLowerCase().replace(/\.$/, ''); }
            var siteNames = new Set(draft.h2_sites.map(function(site) { return normalized(site.domain); }));
            draft.dns_rules.forEach(function(rule) {
                var name = normalized(rule.name);
                if (siteNames.has(name)) throw new Error(_('域名配置冲突：%s 同时出现在 HTTPS 站点和 DNS 规则中，请只保留一处同名配置。').format(name));
            });
            return draft;
		}

		function reset(snapshot) {
			current = snapshot;
			var scope = snapshot.device_scope || {};
			scopeMode.value = scope.mode || 'all';
			scopeAddresses.value = (scope.addresses || []).join('\n');
			scopeAddresses.disabled = scopeMode.value !== 'selected';
			dnsList.replaceChildren(...(snapshot.dns_rules || []).map(dnsRow));
			siteList.replaceChildren(...(snapshot.h2_sites || []).map(siteRow));
			dirty = false;
			saveButton.disabled = discardButton.disabled = true;
			controls.disabled = !canEdit || !!snapshot.monitor_only;
            feedback.textContent = snapshot.monitor_only ? _('仅监控文件：全部设备正常解析；不配置 DNS 改写、黑名单或 HTTPS JSON。可在高级 YAML 查看，或选择其他文件编辑拦截规则。') : canEdit ? _('修改后即可保存。') : _('当前账号只有查看权限。');
		}

		var controls = E('fieldset', { 'class': 'fsd-form-controls' }, [
			E('div', { 'class': 'fsd-actions fsd-form-actions' }, [saveButton, discardButton]),
			feedback,
			E('h4', {}, _('作用设备')),
			field(_('作用范围'), scopeMode),
			field(_('设备 IP 地址（每行一个，最多 64 个）'), scopeAddresses),
			E('p', {}, _('指定设备模式只对列表内地址应用 DNS、JSON 接口、黑名单和出网保护。IPv4 建议绑定 DHCP 静态租约；设备使用 IPv6 时，需同时填写其 IPv6 地址，地址变化后需更新。监控仍显示全部设备。')),
			E('p', {}, _('切换后客户端可能仍缓存旧 DNS，需要刷新 DNS 或重新连接网络。此设置不进行原站反向代理；非目标设备访问旧缓存中的 FSD 地址不会获得配置的 JSON。')),
			E('div', { 'class': 'fsd-form-heading' }, [E('h4', {}, _('DNS 规则')), addDNS]),
 E('p', {}, _('每个域名选择指定 IP、上游解析或阻断。精确域名优先；通配规则按后缀更具体的优先，与排列顺序无关。*.a.com 匹配任意层级子域名，不包含 a.com。同一域名只允许一条规则。')),
 E('p', {}, _('DNS 规则与 HTTPS 自动映射共同匹配：精确域名优先，其次更具体的通配后缀；同名配置不允许同时存在。均未命中时交给上游解析。监控页的临时拦截仍优先。上游解析不进行 HTTPS 反向代理。保存后应用此 YAML 才生效。')),
 dnsList,
			E('div', { 'class': 'fsd-form-heading' }, [E('h4', {}, _('HTTPS 站点')), addSite]),
 E('p', {}, _('每个站点分别配置域名与证书。域名自动映射到设备 LAN IP，并与 DNS 规则共同按匹配精度排序，同名配置不允许同时存在；*.a.com 匹配任意层级子域名，但普通通配证书只覆盖一层。')),
 E('p', {}, _('cert 和 key 仅接受设备上的绝对文件路径，须同时填写或同时留空；留空时应用配置会使用自动生成的自签证书。此处仅显示路径，不读取私钥内容。正常模式需保留 1 至 16 个站点。修改后先保存文件，再应用生效。')),
 siteList
		]);
		controls.addEventListener('input', function(event) {
			var card = event.target.closest('.fsd-route-card');
			if (card)
				changedRoutes.add(card);
			changed();
		});
		scopeMode.addEventListener('change', function() {
			scopeAddresses.disabled = scopeMode.value !== 'selected';
			changed();
		});
		controls.disabled = !canEdit;
		reset(initial);
		return {
			node: controls,
			reset: reset,
			isDirty: function() { return dirty; },
			setBusy: function(busy) { controls.disabled = busy || !canEdit || !!current.monitor_only; }
		};
	}
});
