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
		var changedRoutes = new WeakSet();
		var scopeMode = E('select', { 'class': 'cbi-input-select' }, [
			E('option', { 'value': 'all' }, _('全部设备')),
			E('option', { 'value': 'selected' }, _('指定设备'))
		]);
		var scopeAddresses = input('fsd-scope-addresses', '', '192.168.50.100\nfd00::100', true);
		scopeAddresses.rows = 4;
		var dnsList = E('div', { 'class': 'fsd-form-list' });
		var routeList = E('div', { 'class': 'fsd-form-list' });
		var blockedDomains = input('fsd-blacklist-block', '', 'example.com\n*.example.net', true);
		var allowedDomains = input('fsd-blacklist-allow', '', 'api.example.com', true);
		blockedDomains.rows = allowedDomains.rows = 5;
		var feedback = E('div', { 'class': 'fsd-editor-feedback', 'role': 'status' });
		var saveButton = action(_('保存并应用'), async function() {
			try {
				var draft = readDraft();
				var encoded = JSON.stringify(draft);
				if (new Blob([JSON.stringify({ draft: encoded })]).size > 60 * 1024)
					throw new Error(_('本次修改的数据过多，请分次保存大型 JSON 接口。'));
				reset(await save(encoded));
				feedback.textContent = _('设备范围、DNS、持久黑白名单和 HTTPS JSON 接口已更新');
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

		var addDNS = action(_('添加 DNS 记录'), function() {
			add(dnsList, dnsRow({ name: '', ip: 'ap' }), 16, _('最多可添加 16 条 DNS 记录'));
		});
		var addRoute = action(_('添加接口'), function() {
			add(routeList, routeCard({ path: '', fields: [] }), 8, _('最多可添加 8 个接口'));
		});

		function dnsRow(record) {
			var row = E('div', { 'class': 'fsd-dns-row' });
			row.append(
				field(_('域名'), input('fsd-dns-name', record.name, 'demo.home.arpa'), 'fsd-dns-domain'),
				field(_('IPv4 或 ap'), input('fsd-dns-ip', record.ip, 'ap')),
				remove(_('移除记录'), row, addDNS)
			);
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

		function routeCard(route) {
			var card = E('details', { 'class': 'fsd-route-card' });
			card.open = route.path === '/feature_configuration';
			if (route.path)
				originalRoutes.set(card, route.path);
			var summary = E('summary', {}, route.path || _('新接口'));
			var path = input('fsd-route-path', route.path, '/api/demo');
			path.addEventListener('input', function() { summary.textContent = path.value || _('新接口'); });
			var body = E('div', { 'class': 'fsd-route-body' }, field(_('接口路径（* 匹配一段非空路径，精确规则优先）'), path),
				E('p', {}, _('例如 /b/*/a 匹配 /b/123/a，不匹配 /b/x/y/a。区分大小写和末尾斜杠；通配符较少的规则优先，同等规则按配置顺序匹配。')));
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
			if (originalPath && !changedRoutes.has(card))
				return { path: path, source: originalPath };
			var raw = card.querySelector('.fsd-json-raw');
			if (raw)
				return { path: path, json: jsonValue(raw) };
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
			return { path: path, json: '{' + parts.join(',') + '}' };
		}

		function readDomains(node) {
			var rules = node.value.split(/\r?\n/).map(function(line) { return line.trim(); }).filter(Boolean);
			if (rules.length > 128) {
				node.focus();
				throw new Error(_('黑名单和白名单各最多 128 条，每行填写一条规则。'));
			}
			return rules;
		}

		function readDraft() {
			return {
				revision: current.revision,
				device_scope: { mode: scopeMode.value, addresses: scopeAddresses.value.split(/\r?\n/).map(function(v) { return v.trim(); }).filter(Boolean) },
				dns_blacklist: { block: readDomains(blockedDomains), allow: readDomains(allowedDomains) },
				dns: Array.from(dnsList.children, function(row) {
					return {
						name: row.querySelector('.fsd-dns-name').value.trim(),
						ip: row.querySelector('.fsd-dns-ip').value.trim()
					};
				}),
				h2_routes: Array.from(routeList.children, readRoute)
			};
		}

		function reset(snapshot) {
			current = snapshot;
			var scope = snapshot.device_scope || {};
			scopeMode.value = scope.mode || 'all';
			scopeAddresses.value = (scope.addresses || []).join('\n');
			scopeAddresses.disabled = scopeMode.value !== 'selected';
			var blacklist = snapshot.dns_blacklist || {};
			blockedDomains.value = (blacklist.block || []).join("\n");
			allowedDomains.value = (blacklist.allow || []).join("\n");
			dnsList.replaceChildren(...snapshot.dns.map(dnsRow));
			routeList.replaceChildren(...snapshot.h2_routes.map(routeCard));
			dirty = false;
			saveButton.disabled = discardButton.disabled = true;
			feedback.textContent = canEdit ? _('修改后即可保存。') : _('当前账号只有查看权限。');
		}

		var controls = E('fieldset', { 'class': 'fsd-form-controls' }, [
			E('div', { 'class': 'fsd-actions fsd-form-actions' }, [saveButton, discardButton]),
			feedback,
			E('h4', {}, _('作用设备')),
			field(_('作用范围'), scopeMode),
			field(_('设备 IP 地址（每行一个，最多 64 个）'), scopeAddresses),
			E('p', {}, _('指定设备模式只对列表内地址应用 DNS、JSON 接口、黑名单和出网保护。IPv4 建议绑定 DHCP 静态租约；设备使用 IPv6 时，需同时填写其 IPv6 地址，地址变化后需更新。监控仍显示全部设备。')),
			E('p', {}, _('切换后客户端可能仍缓存旧 DNS，需要刷新 DNS 或重新连接网络。此设置不进行原站反向代理；非目标设备访问旧缓存中的 FSD 地址不会获得配置的 JSON。')),
			E('div', { 'class': 'fsd-form-heading' }, [E('h4', {}, _('DNS 记录')), addDNS]),
			dnsList,
			E('h4', {}, _('持久 DNS 黑白名单')),
			E('p', {}, _('保存后立即生效，服务或设备重启后保留。每行一条，不填写 https:// 或接口路径；留空表示清空对应列表。')),
			field(_('黑名单（block）'), blockedDomains),
			E('p', {}, _('普通域名同时拦截其子域名，也支持 * 和 ? 通配符。')),
			field(_('白名单（allow）'), allowedDomains),
			E('p', {}, _('白名单仅精确匹配域名，不支持通配符。优先级：监控页临时黑名单 > 此白名单 > 此黑名单；白名单不能覆盖临时拦截。')),
			E('div', { 'class': 'fsd-form-heading' }, [E('h4', {}, _('HTTPS JSON 接口')), addRoute]),
			routeList
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
			setBusy: function(busy) { controls.disabled = busy || !canEdit; }
		};
	}
});
