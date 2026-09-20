// msg-web — Web 可视化测试工具
// 浏览器 WebTransport/WebSocket 直连 clover 网关，零代理开销
//
// 连接流程（Web 客户端）：
//  1. 检测浏览器支持
//  2. WebTransport 支持检测
//  3. WebTransport 连接尝试
//  4. WebSocket 连接尝试（回退）

const $ = (s) => document.querySelector(s);

// ========== 状态 ==========
let transport = null;       // WebTransport 或 WebSocket
let transportType = '';     // 'WebTransport' 或 'WebSocket'
let connected = false;
let messages = [];
let selMsg = null;
let reconnectTimer = null;
const MAX_AUTO_RECONNECT = 10;        // 连续自动重连上限，超过即停止，等待手动“连接”
const RECONNECT_BASE_DELAY = 2000;    // 首次自动重连延迟（毫秒）；之后按 2 倍指数退避
const RECONNECT_MAX_DELAY = 30000;    // 退避上限（毫秒）：证书/配置类故障会持续失败，固定 2s 只会刷屏
const STABLE_CONNECT_MS = 5000;       // 连接连续存活满该时长才算“稳定”，此后重连计数才归零
let reconnectAttempts = 0;            // 当前连续重连次数（“即连即断”时持续累加）
let lastConnectedAt = 0;              // 最近一次连接建立时间戳（毫秒），仅用于日志
let stableTimer = null;               // 稳定存活计时器：满 STABLE_CONNECT_MS 才清零 reconnectAttempts
let requestID = 0;
const pending = {}; // requestID → {msgID, name}，回包（msgID=0）时按 requestID 反查消息名
// WebTransport 可靠双向流收发状态
let wtStream = null;
let wtWriter = null;
let wtKeepTimer = null;
// 连接目标地址（来自配置，无输入框，打开页面自动连接）
let connWS = '';
let connWT = '';

const api = {
  async get(url) {
    const res = await fetch(url);
    return res.json();
  },
  async post(url) {
    const res = await fetch(url, { method: 'POST' });
    return res.json();
  },
  // 账号服代理：注册 / 登录（action = 'login' | 'signup'）。
  // 经本工具后端转发而非页面直连账号服——账号服在另一个端口，页面 fetch 属跨域；
  // 后端路径见 main.go 的 handleAuthProxy。返回 { success, owner, token, err }。
  // 账号服是登录链路的必经依赖（游戏服不收账号密码），故 auth_enabled 恒为真。
  async auth(action, account, password) {
    const res = await fetch(`/api/auth/${action}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ account, password }),
    });
    return res.json();
  },
};

// 引擎内核消息号（与 msg-client 的 client 包一致：内核固定，不走 /api/messages 解析）。
const EMsgLogin = 2;
const EMsgResumeSession = 3;
const EPushPlayerFullSync = 4001;

// auth 登录态。与 msg-client 的 loginMemo 同义：缓存账号密码是为了
// resume 失效（JWT 过期 / 服务端重启）时能自动重走一遍完整登录。
const auth = {
  account: '',
  password: '',
  token: '',          // 账号服签发的 JWT
  owner: '',          // 登录成功后的 owner
  sessionToken: '',   // EPushPlayerFullSync 下发，重连优先用它恢复会话
  playerID: '',
  loggedIn: false,    // 本次会话是否已发起过登录（决定重连时是否自动恢复）
};

// ========== 初始化 ==========
async function init() {
  await loadConfig();
  await loadMessages();

  $('#btnConnect').addEventListener('click', onConnect);
  $('#btnDisconnect').addEventListener('click', onDisconnect);
  $('#btnSend').addEventListener('click', onSend);
  $('#btnReload').addEventListener('click', onReload);
  $('#inpFilter').addEventListener('input', renderMsgList);
  $('#msgList').addEventListener('click', onMsgSelect);
  $('#inpMsgID').addEventListener('input', onMsgIDInput);
  $('#chkManual').addEventListener('change', toggleManualJSON);
  // 登录区：登录 / 注册；密码框回车即登录。
  $('#btnLogin').addEventListener('click', () => doAuth('login'));
  $('#btnSignup').addEventListener('click', () => doAuth('signup'));
  $('#inpPassword').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); doAuth('login'); }
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    const el = document.activeElement;
    if (!el || el.tagName === 'BUTTON') return;
    // 登录区输入框的 Enter 归登录（见上面的监听），不能当成「发送」。
    if (el.classList && el.classList.contains('auth-input')) return;
    e.preventDefault();
    onSend();
  });

  // 记录连接目标地址（来自配置，不依赖输入框），随后自动走连接流程。
  connWS = wsURL(window.appCfg && window.appCfg.addr);
  connWT = wtURL(window.appCfg && window.appCfg.addr);
  warnSchemeMismatch();

  autoConnect();
}

// ========== 配置 & 消息列表 ==========
async function loadConfig() {
  try {
    const data = await api.get('/api/config');
    window.appCfg = data;
  } catch (e) {
    // 忽略
  }
}

async function loadMessages() {
  try {
    const data = await api.get('/api/messages');
    messages = data.messages || [];
    if (messages.length === 0) {
      addLogSystem(`WARN 消息列表为空，请检查 proto 配置`);
    }
    renderMsgList();
  } catch (e) {
    messages = [];
    addLogSystem(`ERROR 加载消息列表失败: ${e.message}`);
  }
}

async function onReload() {
  const btn = $('#btnReload');
  btn.disabled = true;
  btn.textContent = '...';
  try {
    const data = await api.post('/api/reload');
    if (data.ok) {
      addLogSystem(`INFO proto 重新扫描完成: ${data.count} 条消息`);
      await loadMessages();
    } else {
      addLogSystem(`ERROR reload 失败: ${data.error}`);
    }
  } catch (e) {
    addLogSystem(`ERROR reload 失败: ${e.message}`);
  } finally {
    btn.disabled = false;
    btn.textContent = 'reload';
  }
}

function renderMsgList() {
  const filter = ($('#inpFilter').value || '').toLowerCase();
  const list = $('#msgList');
  list.innerHTML = '';
  const c2s = messages.filter(m => !m.name.startsWith('EReply') && !m.name.startsWith('EPush') && !m.name.startsWith('Reply') && !m.name.startsWith('Push'));
  const filtered = c2s.filter(m => {
    if (!filter) return true;
    return m.name.toLowerCase().includes(filter) || String(m.value).includes(filter);
  });
  for (const m of filtered) {
    const div = document.createElement('div');
    div.className = 'msg-item' + (m.is_engine ? ' engine' : '');
    div.dataset.value = m.value;
    div.innerHTML = `<span class="msg-name">${esc(m.name)}</span>` + (m.is_engine ? ` <span class="msg-engine-tag">[engine]</span>` : '');
    if (selMsg && selMsg.value === m.value) div.classList.add('selected');
    list.appendChild(div);
  }
}

function onMsgSelect(e) {
  const item = e.target.closest('.msg-item');
  if (!item) return;
  const v = parseInt(item.dataset.value);
  selMsg = messages.find(m => m.value === v) || null;
  $('#inpMsgID').value = v || '';
  renderMsgList(); // 刷新选中高亮
  if (selMsg) renderParamFields(selMsg);
}

function onMsgIDInput() {
  const v = parseInt($('#inpMsgID').value);
  selMsg = messages.find(m => m.value === v) || null;
  if (selMsg) {
    renderMsgList(); // 刷新高亮
    renderParamFields(selMsg);
  }
}

// ========== 动态参数输入 ==========
function renderParamFields(msg) {
  const container = $('#paramFields');
  const manual = $('#manualJSON');
  const chk = $('#chkManual');

  if (chk.checked) {
    // Raw 模式只显示输入框，不构建任何 JSON
    return;
  }

  const fields = msg.req || [];
  if (fields.length === 0) {
    container.innerHTML = '<div class="param-hint">无请求字段，请使用手动 JSON</div>';
    manual.style.display = 'none';
    return;
  }

  manual.style.display = 'none';
  let html = '';
  for (const f of fields) {
    const placeholder = fieldPlaceholder(f);
    html += `<div class="param-row">
      <label class="param-label">${esc(f.json_key || f.name)} <span class="param-type">${esc(f.type)}</span></label>
      <input type="text" class="param-input" data-key="${esc(f.json_key || f.name)}" placeholder="${esc(placeholder)}" autocomplete="off">
    </div>`;
  }
  container.innerHTML = html;
}

function fieldPlaceholder(f) {
  if (f.is_bool) return 'true / false';
  if (f.is_numeric) return '0';
  return '';
}

function toggleManualJSON() {
  const chk = $('#chkManual');
  const manual = $('#manualJSON');
  const container = $('#paramFields');

  if (chk.checked) {
    container.innerHTML = '';
    manual.style.display = 'block';
    // Raw 模式不构建任何 JSON
  } else {
    manual.style.display = 'none';
    if (selMsg) renderParamFields(selMsg);
  }
}

function buildDefaultJSON(fields) {
  if (!fields || fields.length === 0) return '{}';
  const obj = {};
  for (const f of fields) {
    const key = f.json_key || f.name;
    if (f.is_bool) obj[key] = false;
    else if (f.is_numeric) obj[key] = 0;
    else obj[key] = '';
  }
  return JSON.stringify(obj, null, 2);
}

function getBody() {
  if ($('#chkManual').checked) {
    return $('#inpBody').value;
  }
  // 从动态参数组装 JSON
  const inputs = document.querySelectorAll('#paramFields .param-input');
  if (inputs.length === 0) return '{}';
  const obj = {};
  for (const inp of inputs) {
    const key = inp.dataset.key;
    const val = inp.value.trim();
    if (!key) continue;

    // 根据 selMsg 还原类型
    const f = findField(key);
    if (f) {
      if (f.is_bool) {
        obj[key] = val === 'true' || val === '1';
        continue;
      }
      if (f.is_numeric) {
        if (val === '') { obj[key] = 0; continue; }
        const n = Number(val);
        obj[key] = isNaN(n) ? val : n;
        continue;
      }
    }
    obj[key] = val;
  }
  return JSON.stringify(obj, null, 2);
}

function findField(key) {
  if (!selMsg || !selMsg.req) return null;
  return selMsg.req.find(f => (f.json_key || f.name) === key) || null;
}

// ========== 连接流程 ==========
function onConnect() {
  reconnectAttempts = 0; // 手动连接，重置重连计数
  if (stableTimer) { clearTimeout(stableTimer); stableTimer = null; }
  if (!connWS) autoConnect();
  else doConnect(connWS, connWT);
}

function onDisconnect() {
  reconnectAttempts = 0; // 手动断开，重置重连计数
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  if (stableTimer) { clearTimeout(stableTimer); stableTimer = null; }
  if (transport) {
    if (transportType === 'WebSocket') {
      transport.onclose = null; // 阻止自动重连
      transport.close();
    } else if (transportType === 'WebTransport') {
      const tw = transport;
      transport = null; // wt.closed 回调据此判断是主动断开，不再自动重连
      tw.close();
    }
    transport = null;
  }
  connected = false;
  transportType = '';
  updateWSStatus('disconnected');
  showConnectBtn();
  addLogSystem('已断开连接');
}

function autoConnect() {
  connWS = wsURL(window.appCfg && window.appCfg.addr);
  // WT 与 WS 复用同一端口，仅协议与路径不同，从 addr 自动派生。
  connWT = wtURL(window.appCfg && window.appCfg.addr);
  updateWSStatus('connecting');
  addLogSystem(`自动连接: ${connWS}`);
  doConnect(connWS, connWT);
}

// doConnect 标准连接流程：
// 1. 检测浏览器支持
// 2. WebTransport 支持检测
// 3. WebTransport 连接尝试（成功则用 WT；失败回退）
// 4. WebSocket 连接尝试（成功则用 WS；失败返回错误）
async function doConnect(wsAddr, wtAddr) {
  // 1. 检测浏览器支持
  addLogSystem('检测浏览器 WebTransport 支持...');
  if (typeof WebTransport !== 'undefined') {
    addLogSystem('✓ 浏览器支持 WebTransport');

    // 2. WebTransport 支持检测 + 3. 连接尝试
    if (wtAddr) {
      addLogSystem(`尝试 WebTransport 连接: ${wtAddr}`);
      try {
        await doConnectWebTransport(wtAddr);
        addLogSystem('✓ WebTransport 连接成功');
        return;
      } catch (e) {
        addLogSystem(`✗ WebTransport 连接失败: ${e.message}`);
        addLogSystem('回退到 WebSocket 连接...');
      }
    } else {
      addLogSystem('未配置 WebTransport 地址，直接使用 WebSocket...');
    }
  } else {
    addLogSystem('✗ 浏览器不支持 WebTransport');
    addLogSystem('直接使用 WebSocket 连接...');
  }

  // 4. WebSocket 连接尝试
  doConnectWebSocket(wsAddr);
}

// doConnectWebTransport 尝试 WebTransport 连接
async function doConnectWebTransport(addr) {
  // wtURL 已派生完整 https://host:port/wt 地址；直接使用，不能再剥路径、也不能改协议。
  //（WebTransport 标准 URL 是 https://，在 https 页面下自动派生，无需在此强转。）
  if (!/^https?:\/\//.test(addr)) {
    addr = 'https://' + addr;
  }

  const wt = new WebTransport(addr, wtOptions());

  await wt.ready;

  transport = wt;
  transportType = 'WebTransport';
  markConnected(`已通过 WebTransport 连接 ${addr}`);

  // 可靠双向流：服务器端可靠下行也走该流，读循环持续接收。
  wtStream = await wt.createBidirectionalStream();
  wtWriter = wtStream.writable.getWriter();
  readWebTransportStream(wtStream);
  // 不可靠数据报通道（服务器端不可靠推送经 datagram 下发，与 CLI 的 QUIC Datagram 对称）。
  readWebTransportDatagrams(wt);
  // 空闲保活：每 25s 写一个最小帧，刷新服务器 idle 计时（cleanLoop 默认 60s 回收空闲连接）。
  wtKeepTimer = setInterval(wtSendKeepalive, 25000);
  // 连接关闭时清理流与保活定时器，并恢复状态、触发自动重连（与 WebSocket onclose 对齐；
  // 主动断开时 onDisconnect 已置 transport=null，据此跳过重连）。
  wt.closed.then(() => {
    if (wtKeepTimer) { clearInterval(wtKeepTimer); wtKeepTimer = null; }
    if (wtWriter) { try { wtWriter.close(); } catch (_) {} wtWriter = null; }
    wtStream = null;
    if (transport !== wt) return; // 主动断开，不再自动重连
    transport = null;
    markDisconnected('WebTransport 连接已断开');
  });
}

// readWebTransportDatagrams 持续读取 WebTransport 不可靠数据报（datagram）：
// 服务器端对 WT 会话的不可靠推送（位置等高频数据）优先走 datagram，按客户端帧解析后投递。
function readWebTransportDatagrams(wt) {
  const reader = wt.datagrams.readable.getReader();
  function pump() {
    reader.read().then(({ done, value }) => {
      if (done) return;
      if (value && value.byteLength >= 8) {
        handleIncomingData(new Uint8Array(value).buffer);
      }
      pump();
    }).catch(() => { /* datagram 通道关闭即停止 */ });
  }
  pump();
}

// readWebTransportStream 持续读取 WebTransport 可靠双向流
function readWebTransportStream(stream) {
  const reader = stream.readable.getReader();
  function pump() {
    reader.read().then(({ done, value }) => {
      if (done) return;
      if (value && value.byteLength >= 8) {
        handleIncomingData(new Uint8Array(value).buffer);
      }
      pump();
    }).catch(err => {
      addLogSystem(`WebTransport 读取错误: ${err.message}`);
    });
  }
  pump();
}

// wtOptions 构造 WebTransport 连接选项。
//
// 自签名证书场景必须携带 serverCertificateHashes，否则必然失败：
// Chromium 对 WebTransport 的证书校验独立于普通 HTTPS，既不接受本机自建根证书，
// --ignore-certificate-errors 也对其无效。W3C 为此规定了证书固定通路，即对
// 服务器证书 DER 做 SHA-256 并在建连时校验，从而完全绕开 PKI。
// 哈希由网关经受信任的 wss 通道下发（/wt-cert-hash），由 msg-web 随 /api/config 转发。
//
// 未拿到哈希时不设置该字段：此时走标准 PKI，适用于公共 CA 证书的部署。
function wtOptions() {
  const opts = { allowPooling: false }; // 证书固定仅支持独占连接
  const hex = window.appCfg && window.appCfg.cert_hash;
  if (!hex || typeof hex !== 'string' || !/^[0-9a-fA-F]{64}$/.test(hex)) {
    if (hex) {
      addLogSystem(`忽略无效证书哈希: ${hex}`);
    } else {
      // 没有哈希只能走标准 PKI；网关用自签 WT 证书时 Chromium 必然拒绝（这正是
      // “Opening handshake failed”最常见的成因），故把判断与排查方向直接写进日志。
      addLogSystem('⚠ 未取得 WebTransport 证书哈希：将走标准 PKI 校验，自签证书必然握手失败。');
      addLogSystem('  需网关 gateway.enable_wt: true，且 WS 端口能取到 /wt-cert-hash（当前 cert_hash 为空）。');
    }
    return opts;
  }
  const bytes = new Uint8Array(32);
  for (let i = 0; i < 32; i++) bytes[i] = parseInt(hex.substr(i * 2, 2), 16);
  opts.serverCertificateHashes = [{ algorithm: 'sha-256', value: bytes }];
  addLogSystem(`已固定服务器证书 sha256:${hex.slice(0, 16)}…`);
  return opts;
}

// wtSendKeepalive 保活：向流写入 [requestID=0][msgID=0][空 body] 最小帧，
// 仅用于刷新服务器 idle 计时（服务器 readLoop 读到数据即 touch），避免空闲超时回收连接。
function wtSendKeepalive() {
  if (!wtWriter || !connected || !wtStream) return;
  try {
    const buf = new ArrayBuffer(8); // 4B requestID + 4B msgID
    wtWriter.write(new Uint8Array(buf));
  } catch (_) {}
}

// doConnectWebSocket 尝试 WebSocket 连接
function doConnectWebSocket(addr) {
  addLogSystem(`尝试 WebSocket 连接: ${addr}`);
  try {
    transport = new WebSocket(addr);
  } catch (e) {
    addLogSystem(`✗ WebSocket 创建失败: ${e.message}`);
    updateWSStatus('error', '无效地址');
    scheduleReconnect();
    return;
  }
  transportType = 'WebSocket';
  transport.binaryType = 'arraybuffer';

  transport.onopen = () => {
    markConnected(`✓ WebSocket 连接成功: ${addr}`);
  };

  transport.onmessage = (evt) => {
    if (!(evt.data instanceof ArrayBuffer)) return;
    handleIncomingData(evt.data);
  };

  transport.onclose = (evt) => {
    // 只打「连接已断开」无法区分原因，把 close code 一并打出：
    // 1006 = 异常关闭（未收到关闭帧），常见于 TLS 握手被拒（wss 打到明文端口）或被中间层截断；
    // 1000/1001 = 正常关闭。
    const detail = (evt && evt.code) ? `，close code=${evt.code}${evt.reason ? ' reason=' + evt.reason : ''}` : '';
    markDisconnected(`连接已断开${detail}`);
  };

  transport.onerror = () => {
    updateWSStatus('error');
    addLogSystem('✗ WebSocket 出错：常见原因 = 网关未开 TLS（wss 打到明文端口）/ 地址端口不通 / 页面与网关协议不一致');
  };
}

// handleIncomingData 处理接收到的数据（WebTransport 和 WebSocket 共用）
// 帧格式: [4B requestID][4B msgID][body]
function handleIncomingData(data) {
  if (data.byteLength < 8) return;
  const view = new DataView(data);
  const reqID = view.getUint32(0, false);
  const rawMsgID = view.getUint32(4, false);
  const bodyBytes = new Uint8Array(data, 8);
  let body = new TextDecoder().decode(bodyBytes);
  let bodyPretty = body;
  try { bodyPretty = JSON.stringify(JSON.parse(body), null, 2); } catch (_) {}

  // 回包 msgID 恒为 0：按 requestID 反查它对应的请求，还原消息名（与 CLI 一致）。
  let msgID = rawMsgID;
  let name;
  if (rawMsgID === 0 && reqID !== 0 && pending[reqID]) {
    const p = pending[reqID];
    msgID = p.msgID;
    name = p.name + ' ·回包';
    delete pending[reqID];
    handleAuthReply(p.msgID, body);
  } else {
    name = findMsgName(msgID);
  }

  // 登录 / 建角成功：引擎推送 EPushPlayerFullSync，携带 player_id 与 session_token。
  // 存下来供断线重连用 EMsgResumeSession 无缝恢复（与 msg-client 的处理一致）。
  if (rawMsgID === EPushPlayerFullSync) {
    try {
      const fs = JSON.parse(body);
      if (fs && fs.session_token) {
        auth.sessionToken = fs.session_token;
        auth.playerID = fs.player_id || '';
        setAuthStatus('已登录 ' + (auth.owner || auth.playerID || ''), 'logged-in');
      }
    } catch (_) {}
  }

  const isEngine = isEngineMsg(msgID);
  addLogRawRecv(name, msgID, bodyPretty, isEngine);
}

// handleAuthReply 处理与登录相关的回包。
// resume 失败（会话过期 / 服务端重启）时自动回退到完整登录，而不是让用户手动重来；
// 清掉 session 一并做，避免下次重连又拿着失效的它去 resume 撞同一堵墙。
function handleAuthReply(reqMsgID, body) {
  if (reqMsgID !== EMsgResumeSession) return;
  try {
    const rr = JSON.parse(body);
    if (rr && rr.success === false) {
      auth.sessionToken = '';
      auth.playerID = '';
      relogin();
    }
  } catch (_) {}
}

// markConnected 连接建立（WS onopen / WT ready）后统一走这里。
//
// 关键：**不在此处立刻清零 reconnectAttempts**。原写在 onopen 里直接归零，于是
// 「连上→立刻被断」的故障每次都能把计数清掉，MAX_AUTO_RECONNECT 永远到不了，
// 表现为无限重连。改为「连续存活满 STABLE_CONNECT_MS 才算稳定」，到点才归零；
// 未满即断则由 markDisconnected 清掉计时器，计数继续累加。
function markConnected(logLine) {
  connected = true;
  lastConnectedAt = Date.now();
  updateWSStatus('connected');
  showDisconnectBtn();
  addLogSystem(logLine);
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  if (stableTimer) clearTimeout(stableTimer);
  stableTimer = setTimeout(() => {
    stableTimer = null;
    reconnectAttempts = 0;
  }, STABLE_CONNECT_MS);

  // 断线重连后自动恢复登录态：新连接没有绑定 owner，不恢复的话后续业务消息
  // 会被网关登录门禁直接拒掉（401）。首次连接不触发——那时用户还没登录。
  // 延到本轮事件循环之后再发：WebTransport 的 writer 可能刚创建，此刻发会丢帧。
  if (auth.loggedIn) {
    setTimeout(() => { if (connected) sendResume(); }, 0);
  }
}

// markDisconnected 连接断开（WS onclose / WT closed）后统一走这里。
function markDisconnected(logLine) {
  connected = false;
  if (stableTimer) { clearTimeout(stableTimer); stableTimer = null; } // 未满稳定期 → 计数保留
  updateWSStatus('disconnected');
  showConnectBtn();
  addLogSystem(logLine);
  scheduleReconnect();
}

// scheduleReconnect 自动重连：指数退避 + 上限保护。
//
// 退避自 2s 起翻倍，封顶 30s。原先固定 2s 重试在「证书 / 配置类」故障下会一直失败
// （如网关未开 TLS、未启用 WT），既刷屏又空转；退避让日志可读，也降低无谓负载。
function scheduleReconnect() {
  if (reconnectTimer) return;
  reconnectAttempts++;
  if (reconnectAttempts > MAX_AUTO_RECONNECT) {
    addLogSystem(`连续自动重连 ${MAX_AUTO_RECONNECT} 次仍即连即断，已停止自动重连（点击“连接”手动重连）`);
    transport = null;
    connected = false;
    updateWSStatus('disconnected');
    showConnectBtn();
    return;
  }
  const delay = Math.min(RECONNECT_BASE_DELAY * Math.pow(2, reconnectAttempts - 1), RECONNECT_MAX_DELAY);
  addLogSystem(`将在 ${Math.round(delay / 1000)}s 后自动重连（第 ${reconnectAttempts}/${MAX_AUTO_RECONNECT} 次）`);
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    addLogSystem('正在重连...');
    doConnect(connWS, connWT);
  }, delay);
}

function showConnectBtn() {
  $('#btnConnect').style.display = '';
  $('#btnDisconnect').style.display = 'none';
}

function showDisconnectBtn() {
  $('#btnConnect').style.display = 'none';
  $('#btnDisconnect').style.display = '';
}

function updateWSStatus(status, text) {
  const el = $('#wsStatus');
  el.className = 'ws-status ' + status;
  const labels = { connecting: '连接中...', connected: '已连接', disconnected: '未连接', error: '连接错误' };
  el.textContent = text || labels[status] || status;
}

// ========== 发送 ==========
function onSend() {
  const msgID = parseInt($('#inpMsgID').value);
  if (!msgID) {
    addLogSystem('请选择消息');
    return;
  }
  const sendName = selMsg ? selMsg.name : '#' + msgID;
  sendRaw(msgID, getBody(), sendName, selMsg ? selMsg.is_engine : undefined);
}

// sendRaw 编码并发送一帧：[4B requestID][4B msgID][body]，并登记回包反查映射。
// 从 onSend 抽出来供登录流程复用——登录用的 EMsgLogin / EMsgResumeSession
// 没有界面上选中的消息，不能走 onSend 那条「先读输入框」的路径。
// 返回本次 requestID；未连接或传输层不可用时返回 0（并记日志）。
function sendRaw(msgID, body, name, isEngine) {
  if (!connected || !transport) {
    addLogSystem('未连接，请先点击连接');
    return 0;
  }
  const engine = isEngine === undefined ? isEngineMsg(msgID) : isEngine;
  let bodyPretty = body;
  try { bodyPretty = JSON.stringify(JSON.parse(body), null, 2); } catch (_) {}

  // 编码帧: [4B requestID big-endian][4B msgID big-endian][body]
  const reqID = requestID !== 0 ? requestID : 1;  // requestID 不可为 0（0=推送）
  pending[reqID] = { msgID, name };                // 记录回包反查映射
  const bodyBytes = new TextEncoder().encode(body);
  const buf = new ArrayBuffer(8 + bodyBytes.length);
  const view = new DataView(buf);
  view.setUint32(0, reqID, false);
  view.setUint32(4, msgID, false);
  new Uint8Array(buf, 8).set(bodyBytes);

  if (transportType === 'WebTransport') {
    // WebTransport 通过可靠双向流发送（服务器端仅读取 stream 数据）
    if (!wtWriter) {
      addLogSystem('WebTransport 连接未就绪');
      return 0;
    }
    wtWriter.write(new Uint8Array(buf));
  } else {
    // WebSocket 直接发送
    transport.send(buf);
  }
  requestID = reqID + 1;

  addLogRawSend(name, msgID, bodyPretty, engine);
  return reqID;
}

// ========== 登录（账号服）==========
// 流程与 msg-client 的 login 命令逐字对齐：
//   ① HTTP POST /api/auth/{login,signup} 换 JWT（经本工具后端代理，避免跨域）
//   ② 长连接发 EMsgLogin{token}
//   ③ 服务端回 EPushPlayerFullSync → 进入已登录态
// 登录后断线重连会自动恢复会话（见 markConnected）。
//
// 与 CLI 的差异：CLI 靠手敲 login 命令触发，页面靠按钮触发；
// 至于「EMsgUDPBindGrant 后建常驻裸 UDP 通道」——WebTransport 的不可靠数据
// 直接走 datagram（与 QUIC 同理），不需要绑定，故此处没有对应逻辑。

// setAuthStatus 更新右上角登录状态。cls: 'logged-out' | 'logged-in' | 'error'。
function setAuthStatus(text, cls) {
  const el = $('#authStatus');
  if (!el) return;
  el.textContent = text;
  el.className = 'auth-status ' + (cls || 'logged-out');
}

// setAuthBusy 登录 / 注册请求期间禁用按钮，避免重复提交。
function setAuthBusy(busy) {
  const login = $('#btnLogin');
  const signup = $('#btnSignup');
  if (login) login.disabled = busy;
  if (signup) signup.disabled = busy;
}

// doAuth 登录 / 注册。账号服注册即签发 token，故两者后续完全一致。
async function doAuth(action) {
  const isSignup = action === 'signup';
  const account = $('#inpAccount').value.trim();
  const password = $('#inpPassword').value;
  if (!account || !password) {
    setAuthStatus('请填账号和密码', 'error');
    return;
  }
  setAuthBusy(true);
  setAuthStatus(isSignup ? '注册中...' : '登录中...');
  try {
    const r = await api.auth(action, account, password);
    if (!r || !r.success || !r.token) {
      setAuthStatus((isSignup ? '注册失败: ' : '登录失败: ') + ((r && r.err) || '账号服无响应'), 'error');
      return;
    }
    auth.account = account;
    auth.password = password;
    auth.token = r.token;
    auth.owner = r.owner || '';
    addLogSystem(`INFO 账号服${isSignup ? '注册' : '登录'}成功 owner=${auth.owner}`);
    sendLogin();
  } catch (e) {
    setAuthStatus('账号服请求异常: ' + e.message, 'error');
  } finally {
    setAuthBusy(false);
  }
}

// sendLogin 用账号服 token 走长连接登录（EMsgLogin 的 body 只有 token）。
function sendLogin() {
  auth.loggedIn = true;
  // 换新 token 登录：旧 session 一并作废，避免下次重连拿过期 session 去 resume。
  auth.sessionToken = '';
  auth.playerID = '';
  setAuthStatus('登录中...', 'logged-out');
  sendRaw(EMsgLogin, JSON.stringify({ token: auth.token }), 'EMsgLogin', true);
}

// sendResume 断线重连后凭 session_token 恢复会话，无需重新登录。
// 没有 session（首次 / 刚换过 token）时直接重新登录。
function sendResume() {
  if (!auth.sessionToken) {
    sendLogin();
    return;
  }
  sendRaw(EMsgResumeSession,
    JSON.stringify({ player_id: auth.playerID, session_token: auth.sessionToken }),
    'EMsgResumeSession', true);
}

// relogin 重走完整两步登录（HTTP 换新 token → EMsgLogin）。
// 用于 resume 失效：JWT 可能已过期，只重发旧 token 会再次失败。
async function relogin() {
  if (!auth.account || !auth.password) {
    sendLogin();
    return;
  }
  addLogSystem('INFO 会话已失效，自动重新登录...');
  try {
    const r = await api.auth('login', auth.account, auth.password);
    if (r && r.success && r.token) {
      auth.token = r.token;
      auth.owner = r.owner || auth.owner;
      sendLogin();
      return;
    }
    auth.loggedIn = false;
    setAuthStatus('登录已失效，请手动重登', 'error');
    addLogSystem('ERROR 自动重新登录失败: ' + ((r && r.err) || '账号服无响应'));
  } catch (e) {
    auth.loggedIn = false;
    setAuthStatus('登录已失效，请手动重登', 'error');
    addLogSystem('ERROR 自动重新登录异常: ' + e.message);
  }
}

// ========== 日志 ==========
function addLogSystem(text) {
  const el = $('#sendLog');
  const item = document.createElement('div');
  item.className = 'log-entry sys';
  item.innerHTML = `<div class="log-line log-sys">
    <span class="log-tag sys">INFO</span>
    <span class="log-time">${fmtTime()}</span>
    <span>${esc(text)}</span>
  </div>`;
  el.appendChild(item);
  trimLog(el);
  item.scrollIntoView({ block: 'end', behavior: 'instant' });
}

function addLogRawSend(name, msgID, body, isEngine) {
  const el = $('#sendLog');
  const item = document.createElement('div');
  item.className = 'log-entry ok';
  const cls = isEngine ? 'log-msg-name engine' : 'log-msg-name';
  item.innerHTML = `<div class="log-line log-send">
    <span class="log-tag ok">SEND</span>
    <span class="log-time">${fmtTime()}</span>
    <span class="${cls}">${esc(name)} #${msgID}</span>
  </div>
  <pre class="log-data">${esc(body)}</pre>`;
  el.appendChild(item);
  trimLog(el);
  item.scrollIntoView({ block: 'end', behavior: 'instant' });
}

function addLogRawRecv(name, msgID, body, isEngine) {
  const el = $('#sendLog');
  const item = document.createElement('div');
  item.className = 'log-entry ok';
  const cls = isEngine ? 'log-msg-name engine' : 'log-msg-name';
  item.innerHTML = `<div class="log-line log-recv">
    <span class="log-tag recv">RECV</span>
    <span class="log-time">${fmtTime()}</span>
    <span class="${cls}">${esc(name)} #${msgID}</span>
  </div>
  <pre class="log-data">${esc(body)}</pre>`;
  el.appendChild(item);
  trimLog(el);
  item.scrollIntoView({ block: 'end', behavior: 'instant' });
}

function autoScroll(el) {
  requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
}

function fmtTime() {
  const d = new Date();
  const t = d.toLocaleTimeString('zh-CN', { hour12: false });
  const ms = String(d.getMilliseconds()).padStart(3, '0');
  return `${t}.${ms}`;
}

function trimLog(el) {
  while (el.children.length > 80) el.removeChild(el.firstChild);
}

// ========== 工具 ==========
function esc(s) {
  if (!s) return '';
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function findMsgName(msgID) {
  const m = messages.find(x => x.value === msgID);
  return m ? m.name : '#' + msgID;
}

// 页面是否 HTTPS：决定派生协议。网关开 TLS 后 WS/WT 走 wss/https，明文走 ws/http。
// 依据页面自身协议自动派生（msg-web 只配单一 addr，不单独配 ws_addr/wt_addr）。
function pageIsSecure() {
  return location.protocol === 'https:';
}

// wsURL 从 host:port 派生 WebSocket 地址：127.0.0.1:8001 → ws://127.0.0.1:8001/ws。
//
// 协议按**网关实际协议**派生（后端探测结果 appCfg.gateway_scheme），而不是只看页面协议：
// demo 默认给网关留空 tls_cert（保证 Unity 裸 TCP 客户端可用），此时页面若是 https，
// 派生出的 wss:// 会打到明文端口，浏览器只报「握手失败」，从页面看不出原因。
function wsURL(addr) {
  const a = addr || '127.0.0.1:8001';
  const gw = window.appCfg && window.appCfg.gateway_scheme;
  const secure = gw ? gw === 'https' : pageIsSecure();
  return (secure ? 'wss://' : 'ws://') + a + '/ws';
}

// wtURL 从 host:port 派生 WebTransport 地址：127.0.0.1:8001 → https://127.0.0.1:8001/wt
//（WT 与 WS 复用同端口号；WebTransport 标准 URL 恒为 https://，与 WS 侧是否加密无关——
// WT 走 UDP，用自己那张短有效期 ECDSA 证书 + serverCertificateHashes 固定）。
// 原先按页面协议给出 http:// 的写法会让 new WebTransport('http://…') 直接抛错，白跑一次尝试。
function wtURL(addr) {
  const a = addr || '127.0.0.1:8001';
  return 'https://' + a + '/wt';
}

// warnSchemeMismatch 页面协议与网关 WS 协议不一致时，浏览器侧两条通道都会失败：
// 页面 https + 网关明文 → wss 打明文端口被拒、ws:// 又被当混合内容拦截；
// 页面 http + 网关开 TLS → ws 打 TLS 端口同样失败。
// 这种错配在页面上只表现为「握手失败 / 连接已断开」，故先给结论与两条修复路径。
function warnSchemeMismatch() {
  const gw = window.appCfg && window.appCfg.gateway_scheme;
  if (!gw) return; // 未探测到（网关未启动），不判断
  const pageSecure = pageIsSecure();
  if (pageSecure === (gw === 'https')) return;
  addLogSystem(`⚠ 协议不匹配：页面 = ${pageSecure ? 'https' : 'http'}，网关 WS = ${gw}（后端探测）`);
  if (pageSecure) {
    addLogSystem('  二选一：① 给网关配 gateway.tls_cert / tls_key → wss 与 WebTransport 均可用；');
    addLogSystem('          ② 以 web.exe -http 重启（页面 http + ws:// 可用，WebTransport 不可用）。');
  } else {
    addLogSystem('  二选一：① 给 msg-web 配 certs（https 页面）；② 关掉网关 TLS。');
  }
}

function isEngineMsg(msgID) {
  const m = messages.find(x => x.value === msgID);
  return m ? m.is_engine : false;
}

// ========== 启动 ==========
// init 完成配置加载后会自动执行 autoConnect()（打开页面即自动连接）
init();
