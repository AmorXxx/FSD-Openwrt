'use strict';
'require view';
'require rpc';
'require ui';
'require poll';
'require view.fsd.config-editor-v2 as configEditor';

function method(name, params) {
	var call = rpc.declare({
		object: 'fsd',
		method: name,
		params: params || [],
		reject: true
	});

	return function() {
		return call.apply(null, arguments).then(function(response) {
			if (!response || response.error)
				throw new Error(response && response.error || _('FSD 服务暂不可用'));
			return response;
		});
	};
}

var status = method('status');
var config = method('config');
var profiles = method('profiles');
var profileAction = method('profile_action', ['draft']);
var readEditor = method('editor');
var saveEditor = method('save_editor', [ 'draft' ]);
var apply = method('apply', [ 'yaml' ]);
var validate = method('validate', [ 'yaml' ]);
var monitor = method('monitor');
var block = method('block', [ 'action', 'domain' ]);
var monitorControl = method('monitor_control', [ 'action' ]);
var checks = method('checks', [ 'group' ]);
var bench = method('bench');

function text(value) {
	return document.createTextNode(value == null ? '' : String(value));
}

function table(container, headers, rows) {
	var scrollTop = container.scrollTop;
	var header = E('thead', { 'role': 'rowgroup' }, E('tr', {
		'class': 'tr table-titles', 'role': 'row'
	}, headers.map(function(label) {
		return E('th', { 'class': 'th', 'scope': 'col', 'role': 'columnheader' }, text(label));
	})));
	var body = E('tbody', { 'role': 'rowgroup' }, rows.length ? rows.map(function(row) {
		return E('tr', { 'class': 'tr', 'role': 'row' }, row.map(function(value, index) {
			return E('td', { 'class': 'td', 'role': 'cell' }, [
				E('span', { 'class': 'fsd-cell-label', 'aria-hidden': 'true' }, text(headers[index])),
				E('span', {}, text(value == null || value === '' ? '—' : value))
			]);
		}));
	}) : [
		E('tr', { 'class': 'tr', 'role': 'row' }, E('td', {
			'class': 'td fsd-empty', 'colspan': headers.length, 'role': 'cell'
		}, text(_('暂无记录'))))
	]);
	container.replaceChildren(E('table', { 'class': 'table fsd-table', 'role': 'table' }, [header, body]));
	container.scrollTop = scrollTop;
}

function units(bytes) {
	bytes = bytes || 0;
	if (bytes >= 1048576)
		return (bytes / 1048576).toFixed(1) + ' MiB';
	if (bytes >= 1024)
		return (bytes / 1024).toFixed(1) + ' KiB';
	return bytes + ' B';
}

function traffic(row, suffix, format) {
	return format(row['up_bytes' + suffix]) + ' / ' + format(row['down_bytes' + suffix]);
}

function button(title, action, disabled) {
	var node = E('button', {
		'class': 'cbi-button cbi-button-action',
		'type': 'button'
	}, text(title));

	node.disabled = !!disabled;
	node.addEventListener('click', async function() {
		node.disabled = true;
		try {
			await action();
		} catch (error) {
			ui.addNotification(null, E('p', {}, text(error.message)), 'error');
		} finally {
			node.disabled = !!disabled;
		}
	});
	return node;
}

function message(str) {
	ui.addNotification(null, E('p', {}, text(str)), 'info');
}

function tab(name, title, children) {
	return E('div', { 'data-tab': name, 'data-tab-title': title }, children);
}

function scrollTable() {
	return E('div', { 'class': 'fsd-table-scroll' });
}

// Keep controls outside the refreshed tables so polling never resets user input.
function monitorFilter(id, placeholder, choices, redraw) {
	function input(suffix, hint) {
		return E('input', {
			'id': id + suffix, 'type': 'search', 'class': 'cbi-input-text',
			'placeholder': hint, 'autocomplete': 'off', 'spellcheck': 'false',
			'autocapitalize': 'off', 'autocorrect': 'off'
		});
	}
	function field(label, control) {
		return E('label', { 'class': 'fsd-form-field', 'for': control.id }, [text(label), control]);
	}
	var client = input('-client', _('例如 192.168.50.100'));
	var domain = input('-domain', _('例如 example.com（包含匹配）'));
	var keyword = input('-keyword', placeholder);
	var count = E('span', { 'class': 'fsd-filter-count', 'role': 'status' });
	var fields = [field(_('设备地址'), client), field(_('域名'), domain), field(_('关键词'), keyword)];
	var selects = choices.map(function(choice) {
		var select = E('select', { 'id': id + '-' + choice.key, 'class': 'cbi-input-select' },
			[E('option', { 'value': '' }, text(_('全部')))].concat((choice.values || []).map(function(value) {
				return E('option', { 'value': value[0] }, text(value[1]));
			})));
		select.addEventListener('change', redraw);
		fields.push(field(choice.label, select));
		return { choice: choice, node: select, signature: '' };
	});
	[client, domain, keyword].forEach(function(control) { control.addEventListener('input', redraw); });
	fields.push(button(_('清除筛选'), function() {
		client.value = domain.value = keyword.value = '';
		selects.forEach(function(select) { select.node.value = ''; });
		redraw();
	}));
	return {
		node: E('div', { 'class': 'fsd-filter-panel' }, [
			E('div', { 'class': 'fsd-filter-controls' }, fields), count
		]),
		apply: function(rows, searchable) {
			selects.forEach(function(select) {
				if (select.choice.values)
					return;
				var selected = select.node.value;
				var values = Array.from(new Set(rows.map(function(row) {
					return String(row[select.choice.key] || '');
				}).concat(selected))).filter(Boolean).sort();
				var signature = JSON.stringify(values);
				if (signature !== select.signature) {
					select.node.replaceChildren(E('option', { 'value': '' }, text(_('全部'))),
						...values.map(function(value) { return E('option', { 'value': value }, text(value)); }));
					select.node.value = selected;
					select.signature = signature;
				}
			});
			var device = client.value.trim().toLowerCase();
			var domainPart = domain.value.trim().toLowerCase().replace(/\.$/, '');
			var words = keyword.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
			var filtered = rows.filter(function(row) {
				var haystack = searchable(row).join(' ').toLowerCase();
				return String(row.client || '').toLowerCase().includes(device) &&
					String(row.domain || row.name || '').toLowerCase().replace(/\.$/, '').includes(domainPart) &&
					words.every(function(word) { return haystack.includes(word); }) &&
					selects.every(function(select) {
						return !select.node.value || String(row[select.choice.key] || '') === select.node.value;
					});
			});
			count.textContent = _('匹配 %d / %d 条已加载记录').format(filtered.length, rows.length);
			return filtered;
		}
	};
}

return view.extend({
	handleSave: null,
	handleSaveApply: null,
	handleReset: null,

	load: function() {
		if (window.location.protocol === 'http:') {
			var target = new URL(window.location.pathname, window.location.origin);
			target.protocol = 'https:';
			target.port = '8444';
			window.location.replace(target.href);
			return Promise.resolve(target.href);
		}
		return Promise.all([status(), config(), readEditor().catch(function() { return null; }), profiles()]);
	},

	render: function(data) {
		if (typeof data === 'string')
			return E('div', { 'class': 'cbi-map' }, [
				E('h2', {}, 'FSD'),
				E('p', {}, E('a', {
					'class': 'cbi-button cbi-button-action', 'href': data
				}, text(_('打开 FSD HTTPS 管理页'))))
			]);

		var canWrite = L.hasViewPermission();
		var secure = window.location.protocol === 'https:';
		var canEdit = canWrite && secure;
		var statusLine = E('p', { 'role': 'status' });
		var result = E('p', { 'role': 'status' });
		var editor = E('textarea', {
			'id': 'fsd-config',
			'class': 'cbi-input-textarea',
			'spellcheck': 'false',
			'autocapitalize': 'off',
			'autocorrect': 'off',
			'wrap': 'soft'
		});
		var file = E('input', {
			'type': 'file', 'accept': '.yaml,.yml,application/yaml,text/yaml', 'hidden': ''
		});
		var currentYAML = data[1].yaml;
		var rawReady = true;
		var configBusy = false;
		var rawFeedback = E('div', { 'class': 'fsd-editor-feedback', 'role': 'status' });
		var quickContainer = E('div');
		var quick = null;
		editor.value = currentYAML;
		editor.readOnly = !canEdit;

		async function runConfigAction(action) {
			if (configBusy)
				throw new Error(_('已有配置操作正在进行'));
			configBusy = rawControls.disabled = true;
			profileControls.disabled = true;
			if (quick)
				quick.setBusy(true);
			try {
				return await action();
			} finally {
				configBusy = rawControls.disabled = false;
				profileControls.disabled = !canEdit;
				if (quick)
					quick.setBusy(false);
			}
		}

		function setYAML(value) {
			editor.value = currentYAML = value;
			rawReady = true;
			editor.readOnly = !canEdit;
			rawFeedback.textContent = '';
		}

		function setQuick(snapshot) {
			if (!snapshot) {
				quick = null;
				quickContainer.replaceChildren(E('p', {}, text(_('表单暂不可用，可切换到「高级 YAML」编辑配置。'))));
				return;
			}
			if (quick) {
				quick.reset(snapshot);
				return;
			}
			quick = configEditor.create(snapshot, canEdit, function(draft) {
				return runConfigAction(async function() {
					if (editor.value !== currentYAML)
						throw new Error(_('高级 YAML 有未应用的修改，请先处理该草稿。'));
					var saved = await saveEditor(draft);
					try {
						setYAML((await config()).yaml);
					} catch (error) {
						rawReady = false;
						editor.readOnly = true;
						rawFeedback.textContent = _('表单已保存。请重新读取以更新 YAML。');
					}
					return saved;
				});
			});
			quickContainer.replaceChildren(quick.node);
		}

		function requireCleanForm() {
			if (quick && quick.isDirty())
				throw new Error(_('「配置」标签有未保存的修改，请先保存或放弃表单草稿。'));
		}

		file.addEventListener('change', async function() {
			try {
				await runConfigAction(async function() {
					var candidate = file.files[0];
					if (!candidate)
						return;
					if (!rawReady)
						throw new Error(_('请先重新读取配置'));
					if (candidate.size > 128 * 1024)
						throw new Error(_('YAML 文件上限为 128 KiB'));
					editor.value = await candidate.text();
				});
			} catch (error) {
				ui.addNotification(null, E('p', {}, text(error.message)), 'error');
			} finally {
				file.value = '';
			}
		});

		function showStatus(current) {
			var active = [];
			if (current.wan_ipv4)
				active.push('IPv4');
			if (current.wan_ipv6)
				active.push('IPv6');
			statusLine.textContent = [current.ap_ssid, current.ap_ip, current.upstream === 'sim' ? 'SIM' : 'OpenWrt',
				active.join(' / ') || _('当前无默认路由')].join(' · ');
		}
		showStatus(data[0]);

		function yamlToCheck() {
			if (!rawReady)
				throw new Error(_('请先重新读取配置'));
			if (new Blob([editor.value]).size > 128 * 1024)
				throw new Error(_('YAML 文件上限为 128 KiB'));
			return editor.value;
		}
		var configActions = [
			button(_('验证 YAML'), async function() {
				await validate(yamlToCheck());
				message(_('YAML 验证通过，尚未应用'));
			}, !secure),
			button(_('应用 YAML'), function() {
				return runConfigAction(async function() {
					requireCleanForm();
					await apply(yamlToCheck());
					rawReady = false;
					editor.readOnly = true;
					try {
						setYAML((await config()).yaml);
					} catch (error) {
						rawFeedback.textContent = _('配置已应用。请重新读取以更新 YAML。');
					}
					setQuick(await readEditor().catch(function() { return null; }));
					message(_('配置已应用'));
				});
			}, !canEdit),
			button(_('从文件加载'), function() { file.click(); }, !canEdit),
			button(_('下载 YAML'), async function() {
				var current = await config();
				var blob = new Blob([current.yaml], { type: 'application/yaml' });
				var url = URL.createObjectURL(blob);
				var link = E('a', { 'href': url, 'download': 'fsd.example.yaml' });
				document.body.appendChild(link);
				link.click();
				link.remove();
				setTimeout(function() { URL.revokeObjectURL(url); }, 1000);
			}),
			button(_('重新读取'), function() {
				return runConfigAction(async function() {
					requireCleanForm();
					var current = await Promise.all([config(), readEditor().catch(function() { return null; })]);
					refreshProfiles(await profiles());
					setYAML(current[0].yaml);
					setQuick(current[1]);
				});
			})
		];
		var rawControls = E('fieldset', { 'class': 'fsd-form-controls' }, [
			E('label', { 'for': 'fsd-config' }, text(_('服务配置（YAML）'))),
			E('p', {}, text(_('多域名证书使用 h2_sites 列表；与单域名 h2_site 二选一。证书和私钥不会回显，同域名再次保存会保留已有证书。'))),
			editor, file, rawFeedback, E('div', { 'class': 'fsd-actions' }, configActions)
		]);
		setQuick(data[2]);

		var devices = scrollTable(), connections = scrollTable(), queries = scrollTable();
		var logs = E('pre', { 'class': 'fsd-logs' });
		var monitorLine = E('p', { 'role': 'status' });
		var list = E('p');
		var domain = E('input', {
			'id': 'fsd-domain', 'type': 'text', 'class': 'cbi-input-text',
			'placeholder': 'example.com', 'maxlength': 253,
			'autocapitalize': 'off', 'autocorrect': 'off', 'spellcheck': 'false', 'inputmode': 'url'
		});
		domain.disabled = !canWrite;

		var latestSample = { live: {}, dns: [] };
		var states = { local: _('本地'), forwarded: _('转发'), blocked: _('拦截'), upstream_error: _('上游异常') };
		var connectionFilter = monitorFilter('fsd-connections', _('远端 IP、端口、域名；空格分隔多个关键词'), [
			{ key: 'protocol', label: _('协议') }
		], renderMonitorTables);
		var dnsFilter = monitorFilter('fsd-dns', _('域名、应答 IP；空格分隔多个关键词'), [
			{ key: 'type', label: _('查询类型') },
			{ key: 'result', label: _('结果'), values: Object.keys(states).map(function(key) { return [key, states[key]]; }) }
		], renderMonitorTables);

		function renderMonitorTables() {
			var perSecond = function(value) { return units(value) + '/s'; };
			var live = latestSample.live || {};
			table(connections, [ _('设备'), _('远端'), _('端口'), _('协议'), _('域名'), _('上行 / 下行'), _('速率') ],
				connectionFilter.apply(live.connections || [], function(item) {
					return [item.client, item.remote, item.port, item.protocol, item.domain];
				}).map(function(item) {
					return [item.client, item.remote, item.port, item.protocol, item.domain,
						traffic(item, '', units), traffic(item, '_per_sec', perSecond)];
				}));
			table(queries, [ _('时间'), _('设备'), _('域名'), _('类型'), _('结果'), _('应答地址') ],
				dnsFilter.apply(latestSample.dns || [], function(item) {
					return [item.client, item.name, item.type, item.result, states[item.result], (item.addresses || []).join(' ')];
				}).slice().reverse().map(function(item) {
					return [new Date(item.time).toLocaleTimeString(), item.client, item.name, item.type,
						states[item.result] || item.result, (item.addresses || []).join(', ')];
				}));
		}

		async function showMonitor() {
			var sample = await monitor();
			latestSample = sample;
			var live = sample.live || {};
			var perSecond = function(value) { return units(value) + '/s'; };
			monitorLine.textContent = [
				live.enabled ? _('记录中') : _('已暂停'),
				live.packet_capture ? _('DNS、TLS SNI 和 HTTP Host') : _('DNS 域名关联'),
				live.connection_count > (live.connections || []).length ?
					_('显示最近活动的 %d / %d 条连接').format(live.connections.length, live.connection_count) : ''
			].filter(Boolean).join(' · ');
			list.textContent = (live.temporary_blocks || []).join('，') || _('暂无临时规则');
			table(devices, [ _('地址'), 'MAC', _('主机名'), _('上行 / 下行'), _('速率'), 'DNS' ],
				(live.devices || []).map(function(item) {
					return [item.ip, item.mac, item.hostname, traffic(item, '', units),
						traffic(item, '_per_sec', perSecond), item.dns_queries];
				}));
			renderMonitorTables();
			var logText = sample.logs || _('暂无日志');
			if (logs.textContent !== logText) {
				var follow = logs.scrollHeight - logs.clientHeight - logs.scrollTop < 4;
				logs.textContent = logText;
				if (follow)
					logs.scrollTop = logs.scrollHeight;
			}
		}

		renderMonitorTables();

		var monitorTab = tab('monitor', _('监控'), [
			monitorLine,
			E('div', { 'class': 'fsd-actions' }, [
				button(_('开启记录'), async function() { await monitorControl('enable'); await showMonitor(); }, !canWrite),
				button(_('暂停记录'), async function() { await monitorControl('disable'); await showMonitor(); }, !canWrite),
				button(_('清空计数'), async function() { await monitorControl('reset_perf'); await showMonitor(); }, !canWrite)
			]),
			E('h3', {}, text(_('接入设备'))), devices,
			E('h3', {}, text(_('活动连接'))), connectionFilter.node, connections,
			E('h3', {}, text(_('DNS 查询'))), dnsFilter.node, queries,
			E('h3', {}, text(_('临时域名黑名单'))),
			E('label', { 'for': 'fsd-domain' }, text(_('加入或移除域名及其子域名'))),
			E('div', { 'class': 'fsd-actions fsd-domain-actions' }, [
				domain,
				button(_('加入'), async function() { await block('add', domain.value); await showMonitor(); }, !canWrite),
				button(_('移除'), async function() { await block('del', domain.value); await showMonitor(); }, !canWrite)
			]),
			list, E('h3', {}, text(_('实时日志'))), logs
		]);


		var profileState = data[3];
		var profileSelect = E('select', { 'class': 'cbi-input-select', 'aria-label': _('配置方案') });
		var profileName = E('input', { 'class': 'cbi-input-text', 'placeholder': _('新方案名称'), 'aria-label': _('新方案名称'), 'maxlength': '80' });
		var profileBadge = E('strong', { 'class': 'fsd-profile-active' });
		var profileFeedback = E('p', { 'role': 'status', 'class': 'fsd-editor-feedback' });
		function refreshProfiles(state) {
			profileState = state;
			profileBadge.textContent = _('当前方案：') + state.active;
			profileSelect.replaceChildren(...state.names.map(function(name) { return E('option', {value: name}, name); }));
			profileSelect.value = state.active;
		}
		async function manageProfile(action) {
			return runConfigAction(async function() {
				requireCleanForm();
				if (!rawReady || editor.value !== currentYAML)
					throw new Error(_('请先保存或放弃高级 YAML 草稿，再管理方案。'));
				var name = action === 'switch' || action === 'delete' ? profileSelect.value : profileName.value.trim();
				if (!name) throw new Error(_('请填写新方案名称。'));
				if (action === 'delete' && !window.confirm(_('确定删除方案「%s」？').format(name))) return;
				if (action === 'switch' && name !== profileState.active && !window.confirm(_('切换到「%s」？FSD 配置将立即生效，网络和热点设置保持不变。').format(name))) return;
				profileFeedback.textContent = _('正在处理…');
				try {
					var updated = await profileAction(JSON.stringify({action: action, name: name, source: profileSelect.value, active: profileState.active}));
					refreshProfiles(updated);
					profileName.value = '';
					var fresh = await Promise.all([config(), readEditor()]);
					setYAML(fresh[0].yaml); setQuick(fresh[1]);
					profileFeedback.textContent = _('操作完成，当前使用「%s」。').format(updated.active);
				} catch (error) {
					profileFeedback.textContent = error.message + _(' 如连接曾中断，请重新读取配置确认当前方案。');
					throw error;
				}
			});
		}
		var profileControls = E('fieldset', { 'class': 'fsd-profile-controls' }, [
			E('div', { 'class': 'fsd-profile-heading' }, [E('h3', {}, _('配置方案')), profileBadge]),
			E('p', { 'class': 'fsd-profile-help' }, _('每套方案独立保存。编辑后保存到当前方案；另存不会自动切换。证书等外部文件共用。')),
			E('div', { 'class': 'fsd-profile-picker' }, [profileSelect, button(_('切换并应用'), function() { return manageProfile('switch'); }, !canEdit)]),
			E('div', { 'class': 'fsd-profile-picker' }, [profileName,
				button(_('当前配置另存为'), function() { return manageProfile('save'); }, !canEdit),
				button(_('复制所选'), function() { return manageProfile('copy'); }, !canEdit),
				button(_('重命名所选'), function() { return manageProfile('rename'); }, !canEdit),
				button(_('删除所选'), function() { return manageProfile('delete'); }, !canEdit)]),
			profileFeedback
		]);
		profileControls.disabled = !canEdit;
		refreshProfiles(profileState);
		var diagnosticResults = scrollTable();
		var configTab = tab('config', _('配置'), [
			E('h3', {}, text(_('可视化编辑'))),
			quickContainer
		]);
		var yamlTab = tab('yaml', _('高级 YAML'), [rawControls]);
		var testing = false;
		function runTest(action) {
			return async function() {
				if (testing)
					throw new Error(_('已有测试正在运行'));
				testing = true;
				result.textContent = _('正在测试…');
				diagnosticResults.replaceChildren();
				try {
					await action();
				} catch (error) {
					result.textContent = error.message;
					throw error;
				} finally {
					testing = false;
				}
			};
		}

		function showChecks(rows) {
			var labels = { pass: _('通过'), fail: _('失败'), skip: _('跳过') };
			table(diagnosticResults, [ _('项目'), _('结果'), _('详情') ], rows.map(function(item) {
				return [item.name, labels[item.status] || item.status, item.detail];
			}));
			result.textContent = _('%d 项通过，%d 项失败，%d 项跳过').format(
				rows.filter(function(item) { return item.status === 'pass'; }).length,
				rows.filter(function(item) { return item.status === 'fail'; }).length,
				rows.filter(function(item) { return item.status === 'skip'; }).length);
		}

		function testButton(label, group) {
			return button(label, runTest(async function() {
				showChecks((await checks(group)).results);
			}));
		}

		var diagnosticsTab = tab('diagnostics', _('自检'), [
			E('p', {}, text(_('路由器侧检测直接请求正在运行的服务。开机自启检查会读取启动配置。'))),
			E('div', { 'class': 'fsd-actions' }, [
				testButton(_('一键自检'), 'all'),
				testButton(_('检查开机保护'), 'guard'),
				testButton(_('测试 HTTP/2 与 HTTP/1.1'), 'https'),
				testButton(_('测试自定义接口'), 'routes'),
				testButton(_('测试 DNS 与黑名单'), 'dns'),
				testButton(_('测试 SIM 外网'), 'sim'),
				testButton(_('检查热点、监控与自启'), 'local')
			]),
			E('div', { 'class': 'fsd-actions' }, [
				button(_('短时复测（3 轮）'), runTest(async function() {
					var rows = [];
					for (var round = 1; round <= 3; round++) {
						result.textContent = _('正在执行第 %d / 3 轮…').format(round);
						var sample = await checks('all');
						for (var item of sample.results) {
							rows.push({
								name: _('第 %d 轮').format(round) + ' · ' + item.name,
								status: item.status, detail: item.detail
							});
						}
						showChecks(rows);
						if (round < 3)
							await new Promise(function(resolve) { setTimeout(resolve, 5000); });
					}
				})),
				button(_('测量连接采样耗时'), runTest(async function() {
					var measured = await bench();
					result.textContent = _('%d 条连接，用时 %d µs').format(measured.entries, measured.sample_duration_us);
				}))
			]),
			result, diagnosticResults,
			E('h3', {}, text(_('从客户端验证上网'))),
			E('p', {}, text(_('用测试设备连接随身 Wi-Fi，并停用它的其他上网连接。打开外网站点后，在监控页核对该设备的连接和流量。'))),
			E('a', {
				'class': 'cbi-button cbi-button-action fsd-external-link',
				'href': 'https://www.iana.org/', 'target': '_blank', 'rel': 'noopener noreferrer'
			}, text(_('从本机打开外网站点')))
		]);
		var tabs = [configTab, yamlTab, monitorTab, diagnosticsTab];
		var parent = E('div', { 'class': 'cbi-map fsd-view' }, [
			E('link', { 'rel': 'stylesheet', 'href': L.resource('view/fsd/overview.css') }),
			E('div', { 'class': 'fsd-page-heading' }, [E('h2', {}, 'FSD'), E('span', {}, _('网络服务与配置管理'))]), statusLine, profileControls,
			E('div', { 'class': 'fsd-tabs' }, tabs)
		]);
		ui.tabs.initTabGroup(tabs);
		poll.add(function() {
			return Promise.all([
				status().then(showStatus),
				monitorTab.getAttribute('data-tab-active') === 'true' ? showMonitor() : Promise.resolve()
			]).catch(function(error) {
				statusLine.textContent = error.message;
			});
		}, 3);
		return parent;
	}
});
