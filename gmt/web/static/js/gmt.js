/*
 * gmt.js —— 后台前端的全部公共能力。
 *
 * 设计原则（与老后台最大的不同）：
 *   1. 只有一个文件、一套写法。老后台每个模块一份 JS（列表、弹窗、日期控件各写各的），
 *      改一个交互要改一片文件；这里所有模块共用同一套列表/表单/表格渲染。
 *   2. 页面不写业务。页面只声明「我是哪个模块」，剩下的由后端返回的
 *      模块元信息（列、查询条件、表单字段、按钮）驱动，前端不再重复描述业务。
 *   3. 与后端的唯一约定是 {code, message, data}。code 非 0 一律 reject 成 Error，
 *      调用方只写正常路径，错误统一在 catch 里提示。
 */
window.gmt = (function () {
    'use strict';

    // ---------------- 请求 ----------------

    function request(path, method, body) {
        var opt = { method: method || 'POST', headers: {}, credentials: 'same-origin' };
        if (body !== undefined && body !== null) {
            opt.headers['Content-Type'] = 'application/json';
            opt.body = JSON.stringify(body);
        }
        return fetch(path, opt).then(function (res) {
            return res.json().catch(function () {
                throw new Error('服务返回了非 JSON 内容（状态码 ' + res.status + '）');
            });
        }).then(function (json) {
            if (json.code === 401) {
                location.href = '/login';
                throw new Error(json.message || '未登录');
            }
            if (json.code !== 0) {
                throw new Error(json.message || '操作失败');
            }
            return json.data;
        });
    }

    function post(path, body) { return request(path, 'POST', body === undefined ? {} : body); }
    function get(path) { return request(path, 'GET', null); }

    // ---------------- 提示 ----------------

    function toast(message, icon) {
        if (window.Swal) {
            Swal.fire({ toast: true, position: 'top-end', icon: icon || 'success', title: message, showConfirmButton: false, timer: 2200 });
        } else {
            alert(message);
        }
    }

    function confirm(message) {
        if (!window.Swal) return Promise.resolve(window.confirm(message));
        return Swal.fire({
            title: message,
            icon: 'warning',
            showCancelButton: true,
            confirmButtonText: '确定',
            cancelButtonText: '取消'
        }).then(function (r) { return !!r.isConfirmed; });
    }

    function info(title, html) {
        if (!window.Swal) { alert(title); return; }
        Swal.fire({ title: title, html: html, width: 720 });
    }

    // ---------------- 格式化 ----------------

    function escapeHtml(v) {
        return String(v === null || v === undefined ? '' : v)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function pad(n) { return n < 10 ? '0' + n : '' + n; }

    function fmtTime(unix) {
        if (!unix) return '-';
        var d = new Date(Number(unix) * 1000);
        return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
            ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
    }

    // toUnix 把「2026-01-01 12:00」或时间戳统一成秒级时间戳。
    function toUnix(v) {
        if (v === '' || v === null || v === undefined) return 0;
        if (/^\d+$/.test(String(v))) return Number(v);
        var d = new Date(String(v).replace(/-/g, '/'));
        return isNaN(d.getTime()) ? 0 : Math.floor(d.getTime() / 1000);
    }

    // ---------------- 表单 ----------------

    // renderForm 按字段声明渲染一组表单控件，返回后可用 readForm 取值。
    function renderForm(container, fields, values) {
        values = values || {};
        container.empty();
        fields.forEach(function (f) {
            var id = 'f_' + f.key;
            var req = f.required ? ' <span class="text-danger">*</span>' : '';
            var ph = f.placeholder ? ' placeholder="' + escapeHtml(f.placeholder) + '"' : '';
            var input;

            if (f.type === 'select') {
                var opts = (f.options || []).map(function (o) {
                    return '<option value="' + escapeHtml(o.value) + '">' + escapeHtml(o.label) + '</option>';
                }).join('');
                input = '<select class="form-control" id="' + id + '"' + (f.multiple ? ' multiple' : '') + '>' + opts + '</select>';
            } else if (f.type === 'textarea') {
                input = '<textarea class="form-control" id="' + id + '" rows="3"' + ph + '></textarea>';
            } else if (f.type === 'datetime') {
                input = '<input type="text" class="form-control gmt-dt" id="' + id + '"' + ph + '>';
            } else if (f.type === 'password') {
                input = '<input type="password" class="form-control" id="' + id + '"' + ph + '>';
            } else if (f.type === 'number') {
                input = '<input type="number" class="form-control" id="' + id + '"' + ph + '>';
            } else {
                input = '<input type="text" class="form-control" id="' + id + '"' + ph + '>';
            }

            container.append(
                '<div class="form-group row mb-2">' +
                '<label class="col-sm-3 col-form-label">' + escapeHtml(f.label) + req + '</label>' +
                '<div class="col-sm-9">' + input + '</div>' +
                '</div>'
            );
        });
        fillForm(container, fields, values);
        initWidgets(container);
    }

    function fillForm(container, fields, values) {
        fields.forEach(function (f) {
            var el = container.find('#f_' + f.key);
            var v = values[f.key];
            if (v === undefined || v === null) v = '';
            if (f.type === 'datetime') {
                el.val(v ? fmtTime(v) : '');
            } else if (f.multiple) {
                el.val((v || []).map(String)).trigger('change');
            } else {
                el.val(String(v));
            }
        });
    }

    function readForm(container, fields) {
        var out = {};
        fields.forEach(function (f) {
            var el = container.find('#f_' + f.key);
            if (f.multiple) {
                out[f.key] = el.val() || [];
            } else if (f.type === 'datetime') {
                out[f.key] = toUnix(el.val());
            } else if (f.type === 'number') {
                out[f.key] = el.val() === '' ? 0 : Number(el.val());
            } else {
                out[f.key] = el.val();
            }
        });
        return out;
    }

    // initWidgets 只需对新插入的 DOM 调用一次，避免重复初始化 select2/flatpickr。
    function initWidgets(container) {
        container.find('.gmt-dt').each(function () {
            if (this._fp) return;
            if (window.flatpickr) {
                this._fp = window.flatpickr(this, { enableTime: true, enableSeconds: false, time_24hr: true, dateFormat: 'Y-m-d H:i' });
            }
        });
        if (window.jQuery && jQuery.fn.select2) {
            container.find('select[multiple]').each(function () {
                if (jQuery(this).data('select2')) return;
                jQuery(this).select2({ width: '100%', placeholder: '可多选' });
            });
        }
    }

    // ---------------- 弹窗 ----------------

    // modal 创建一个 Bootstrap 弹窗，onOk 返回 false 时保持打开（用于校验失败）。
    function modal(title, bodyHtml, onOk) {
        var id = 'gmt-modal';
        jQuery('#' + id).remove();
        jQuery('body').append(
            '<div class="modal fade" id="' + id + '" tabindex="-1" role="dialog">' +
            '<div class="modal-dialog modal-lg modal-dialog-centered" role="document">' +
            '<div class="modal-content">' +
            '<div class="modal-header"><h5 class="modal-title">' + escapeHtml(title) + '</h5>' +
            '<button type="button" class="close" data-dismiss="modal"><span>&times;</span></button></div>' +
            '<div class="modal-body">' + bodyHtml + '</div>' +
            '<div class="modal-footer">' +
            '<button type="button" class="btn btn-light" data-dismiss="modal">取消</button>' +
            '<button type="button" class="btn btn-primary" id="gmt-modal-ok">保存</button>' +
            '</div></div></div></div>'
        );
        var box = jQuery('#' + id);
        box.on('click', '#gmt-modal-ok', function () {
            var btn = jQuery(this);
            btn.prop('disabled', true);
            Promise.resolve()
                .then(function () { return onOk(box); })
                .then(function (close) {
                    if (close !== false) box.modal('hide');
                })
                .catch(function (e) { toast(e.message, 'error'); })
                .then(function () { btn.prop('disabled', false); });
        });
        box.modal('show');
    }

    // ---------------- 通用列表 ----------------

    var list = (function () {
        var state = { key: '', schema: null, page: 1, size: 20 };

        function searchValues() {
            var out = {};
            (state.schema.search || []).forEach(function (f) {
                out[f.key] = jQuery('#s_' + f.key).val() || '';
            });
            return out;
        }

        function renderSearch() {
            var box = jQuery('#gmt-search');
            box.empty();
            (state.schema.search || []).forEach(function (f) {
                var id = 's_' + f.key;
                var input;
                if (f.type === 'select') {
                    var opts = (f.options || []).map(function (o) {
                        return '<option value="' + escapeHtml(o.value) + '">' + escapeHtml(o.label) + '</option>';
                    }).join('');
                    input = '<select class="form-control" id="' + id + '">' + opts + '</select>';
                } else {
                    input = '<input type="text" class="form-control" id="' + id + '" placeholder="' + escapeHtml(f.label) + '">';
                }
                box.append('<div class="form-group mr-2 mb-2"><label class="mr-2">' + escapeHtml(f.label) + '</label>' + input + '</div>');
            });
            box.append(
                '<button type="button" class="btn btn-primary mr-2 mb-2" id="gmt-do-search">查询</button>' +
                '<button type="button" class="btn btn-light mb-2" id="gmt-reset">重置</button>'
            );
        }

        function cell(col, row) {
            var raw = row[col.key];
            var key = raw === true || raw === false ? String(raw) : (raw === null || raw === undefined ? '' : String(raw));
            if (col.kind === 'time') {
                var u = Number(raw);
                return u > 0 ? escapeHtml(fmtTime(u)) : '-';
            }
            var text = (col.labels && col.labels[key] !== undefined) ? col.labels[key] : key;
            if (col.kind === 'badge') {
                var cls = (col.badge && col.badge[key]) || 'secondary';
                return '<span class="badge badge-' + cls + '">' + escapeHtml(text) + '</span>';
            }
            return '<span class="gmt-cell-long" title="' + escapeHtml(text) + '">' + escapeHtml(text || '-') + '</span>';
        }

        function render(rows) {
            var cols = state.schema.columns || [];
            var head = cols.map(function (c) {
                return '<th' + (c.width ? ' style="width:' + c.width + '"' : '') + '>' + escapeHtml(c.title) + '</th>';
            });
            var body = jQuery('#gmt-body').empty();

            if (!rows.length) {
                head = [];
                body.append('<tr><td class="text-center text-muted py-4" colspan="99">没有数据</td></tr>');
            }

            jQuery('#gmt-head').empty().append('<tr>' + head.join('') + (rows.length ? '<th class="gmt-nowrap">操作</th>' : '') + '</tr>');

            rows.forEach(function (row) {
                var tds = cols.map(function (c) { return '<td>' + cell(c, row) + '</td>'; });
                var ops = [];
                if (!state.schema.read_only) {
                    ops.push('<button class="btn btn-sm btn-light mr-1" data-op="edit">编辑</button>');
                    ops.push('<button class="btn btn-sm btn-danger mr-1" data-op="del">删除</button>');
                }
                (state.schema.actions || []).forEach(function (a) {
                    ops.push('<button class="btn btn-sm btn-' + (a.style || 'light') + ' mr-1" data-op="act" data-act="' + escapeHtml(a.key) + '">' + escapeHtml(a.name) + '</button>');
                });
                var tr = jQuery('<tr>' + tds.join('') + '<td class="gmt-nowrap">' + ops.join('') + '</td></tr>');
                tr.data('row', row);
                body.append(tr);
            });
        }

        function renderPager(total) {
            var pages = Math.max(1, Math.ceil(total / state.size));
            var box = jQuery('#gmt-pager').empty();
            box.append(
                '<div class="d-flex align-items-center">' +
                '<span class="text-muted mr-3">共 ' + total + ' 条 / ' + pages + ' 页</span>' +
                '<button class="btn btn-sm btn-light mr-1" data-page="' + (state.page - 1) + '"' + (state.page <= 1 ? ' disabled' : '') + '>上一页</button>' +
                '<button class="btn btn-sm btn-light" data-page="' + (state.page + 1) + '"' + (state.page >= pages ? ' disabled' : '') + '>下一页</button>' +
                '</div>'
            );
        }

        function load(page) {
            state.page = page || state.page;
            return post('/api/m/' + state.key + '/list', {
                page: state.page, size: state.size, search: searchValues()
            }).then(function (data) {
                render(data.rows || []);
                renderPager(data.total || 0);
            }).catch(function (e) { toast(e.message, 'error'); });
        }

        function openEdit(row) {
            var values = row ? jQuery.extend({}, row) : {};
            if (!row) {
                (state.schema.form || []).forEach(function (f) { if (f.default !== undefined) values[f.key] = f.default; });
            }
            modal((row ? '编辑' : '新增') + state.schema.name, '<form id="gmt-edit-form"></form>', function (box) {
                var v = readForm(box.find('#gmt-edit-form'), state.schema.form || []);
                if (row) v.id = row.id;
                return post('/api/m/' + state.key + '/save', v).then(function () {
                    toast('保存成功');
                    load(state.page);
                    return true;
                });
            });
            renderForm(jQuery('#gmt-edit-form'), state.schema.form || [], values);
        }

        function bind() {
            var doc = jQuery(document);
            doc.off('.gmt-list');
            doc.on('click.gmt-list', '#gmt-body button[data-op]', function () {
                var row = jQuery(this).closest('tr').data('row');
                var op = jQuery(this).data('op');
                if (op === 'edit') return openEdit(row);
                if (op === 'del') {
                    confirm('确认删除该记录？删除后不可恢复').then(function (ok) {
                        if (!ok) return;
                        post('/api/m/' + state.key + '/delete', { id: row.id })
                            .then(function () { toast('已删除'); load(state.page); })
                            .catch(function (e) { toast(e.message, 'error'); });
                    });
                    return;
                }
                var actKey = jQuery(this).data('act');
                var act = null;
                (state.schema.actions || []).forEach(function (a) { if (a.key === actKey) act = a; });
                if (!act) return;
                var run = function () {
                    post('/api/m/' + state.key + '/action', { action: act.key, id: row.id, form: {} })
                        .then(function (data) {
                            toast('操作成功');
                            if (data && data.codes) {
                                info('生成结果（请自行保存）',
                                    '<textarea class="form-control gmt-codes" readonly>' + escapeHtml((data.codes || []).join('\n')) + '</textarea>');
                            }
                            load(state.page);
                        })
                        .catch(function (e) { toast(e.message, 'error'); });
                };
                if (act && act.confirm) confirm(act.confirm).then(function (ok) { if (ok) run(); });
                else run();
            });
            doc.on('click.gmt-list', '#gmt-pager button[data-page]', function () {
                load(Number(jQuery(this).data('page')));
            });
            doc.on('click.gmt-list', '#gmt-do-search', function () { load(1); });
            doc.on('click.gmt-list', '#gmt-reset', function () {
                jQuery('#gmt-search').find('input,select').val('');
                load(1);
            });
            doc.on('click.gmt-list', '#gmt-create', function () { openEdit(null); });
            doc.on('click.gmt-list', '#gmt-reload', function () { load(state.page); });
        }

        function boot(key) {
            state.key = key;
            bind();
            get('/api/m/' + key + '/schema').then(function (schema) {
                state.schema = schema;
                renderSearch();
                return load(1);
            }).catch(function (e) { toast(e.message, 'error'); });
        }

        return { boot: boot };
    })();

    // ---------------- 概览 ----------------

    var dashboard = (function () {
        var cards = [
            { key: 'servers', label: '区服数', icon: 'layers' },
            { key: 'machines', label: '机器数', icon: 'server' },
            { key: 'active_bans', label: '生效中的封禁', icon: 'lock' },
            { key: 'gifts', label: '礼包批次', icon: 'gift' },
            { key: 'today_ops', label: '今日操作', icon: 'activity' }
        ];

        function render(data) {
            var stats = data.stats || {};
            jQuery('#gmt-stats').empty();
            cards.forEach(function (c) {
                jQuery('#gmt-stats').append(
                    '<div class="col-sm-6 col-xl-3 gmt-stat"><div class="card"><div class="card-body">' +
                    '<div class="gmt-stat-label">' + escapeHtml(c.label) + '</div>' +
                    '<div class="gmt-stat-value">' + escapeHtml(stats[c.key] || 0) + '</div>' +
                    '</div></div></div>'
                );
            });

            var nodes = jQuery('#gmt-nodes').empty();
            (data.nodes || []).forEach(function (n) {
                nodes.append('<tr><td>' + escapeHtml(n.node) + '</td><td>' + escapeHtml(n.addr) + '</td>' +
                    '<td>' + (n.online ? escapeHtml(n.latency) + ' ms' : '-') + '</td>' +
                    '<td>' + (n.online
                        ? '<span class="badge badge-success">在线</span>'
                        : '<span class="badge badge-danger">离线</span>') +
                    (n.message ? ' <small class="text-muted">' + escapeHtml(n.message) + '</small>' : '') + '</td></tr>');
            });

            var recent = jQuery('#gmt-recent').empty();
            if (!(data.recent || []).length) {
                recent.append('<tr><td colspan="4" class="text-center text-muted">暂无操作记录</td></tr>');
            }
            (data.recent || []).forEach(function (a) {
                recent.append('<tr><td class="gmt-nowrap">' + escapeHtml(a.time) + '</td>' +
                    '<td>' + escapeHtml(a.account) + '</td>' +
                    '<td>' + escapeHtml(a.type) + '</td>' +
                    '<td>' + escapeHtml(a.info) + '</td></tr>');
            });
        }

        function load() {
            return get('/api/dashboard').then(render).catch(function (e) { toast(e.message, 'error'); });
        }

        function boot() {
            jQuery(document).on('click', '#gmt-reload', load);
            load();
        }

        return { boot: boot };
    })();

    // ---------------- 玩家查询 ----------------

    var player = (function () {
        var rows = [
            { key: 'player_id', label: '角色 ID' },
            { key: 'name', label: '角色名' },
            { key: 'account', label: '账号' },
            { key: 'server_id', label: '区服' },
            { key: 'level', label: '等级' },
            { key: 'vip', label: 'VIP' },
            { key: 'power', label: '战力' },
            { key: 'guild', label: '军团' },
            { key: 'online', label: '在线' },
            { key: 'create_time', label: '创建时间' },
            { key: 'last_login', label: '最后登录' }
        ];

        function boot() {
            jQuery('#gmt-form').on('submit', function (e) {
                e.preventDefault();
                jQuery('#gmt-result').removeClass('d-none');
                post('/api/player/search', {
                    server_id: Number(jQuery('#gmt-server').val()) || 0,
                    kind: jQuery('#gmt-kind').val(),
                    keyword: jQuery('#gmt-keyword').val()
                }).then(function (p) {
                    var box = jQuery('#gmt-player').empty();
                    rows.forEach(function (r) {
                        var v = p[r.key];
                        if (r.key === 'create_time' || r.key === 'last_login') v = fmtTime(v);
                        if (r.key === 'online') v = v ? '<span class="badge badge-success">在线</span>' : '<span class="badge badge-secondary">离线</span>';
                        box.append('<tr><th style="width:120px">' + escapeHtml(r.label) + '</th><td>' + escapeHtml(v === null || v === undefined ? '-' : v) + '</td></tr>');
                    });
                    var items = jQuery('#gmt-items').empty();
                    if (!(p.items || []).length) {
                        items.append('<tr><td colspan="2" class="text-center text-muted">无</td></tr>');
                    }
                    (p.items || []).forEach(function (it) {
                        items.append('<tr><td>' + escapeHtml(it.config_id) + '</td><td>' + escapeHtml(it.num) + '</td></tr>');
                    });
                }).catch(function (err) {
                    jQuery('#gmt-result').addClass('d-none');
                    toast(err.message, 'error');
                });
            });
        }

        return { boot: boot };
    })();

    // ---------------- 发邮件 ----------------

    var mail = (function () {
        function boot() {
            var sync = function () {
                var all = jQuery('#gmt-all').is(':checked');
                jQuery('#gmt-targets-row').toggleClass('d-none', all);
                jQuery('#gmt-level-row').toggleClass('d-none', !all);
            };
            jQuery('input[name="gmt-target"]').on('change', sync);
            sync();

            jQuery('#gmt-form').on('submit', function (e) {
                e.preventDefault();
                var all = jQuery('#gmt-all').is(':checked');
                var title = jQuery('#gmt-title').val();
                if (!title) { toast('标题不能为空', 'error'); return; }
                var body = {
                    server_id: Number(jQuery('#gmt-server').val()) || 0,
                    all: all,
                    min_level: Number(jQuery('#gmt-level').val()) || 0,
                    targets: jQuery('#gmt-targets').val(),
                    title: title,
                    content: jQuery('#gmt-content').val(),
                    items: jQuery('#gmt-items').val()
                };
                var tip = all ? '确认向全区服发送该邮件？' : '确认向指定玩家发送该邮件？';
                confirm(tip).then(function (ok) {
                    if (!ok) return;
                    post('/api/mail/send', body)
                        .then(function () { toast('已下发'); })
                        .catch(function (err) { toast(err.message, 'error'); });
                });
            });
        }

        return { boot: boot };
    })();

    return {
        request: request, post: post, get: get,
        toast: toast, confirm: confirm, info: info,
        fmtTime: fmtTime, toUnix: toUnix, escapeHtml: escapeHtml,
        renderForm: renderForm, readForm: readForm, modal: modal,
        list: list, dashboard: dashboard, player: player, mail: mail
    };
})();
