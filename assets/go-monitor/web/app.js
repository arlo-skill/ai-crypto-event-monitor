'use strict';
const $ = id => document.getElementById(id);
const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const fmtTime = value => { const d = new Date(value); return Number.isNaN(d.getTime()) ? '尚未确认' : d.toLocaleString('zh-CN', {month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false}); };
const num = value => value === null || value === undefined || value === '' ? null : Number.isFinite(Number(value)) ? Number(value) : null;
const fraction = value => num(value) === null ? '待确认' : `${Number((Number(value)*10).toFixed(2))} 成`;
const percent = value => num(value) === null ? '待确认' : `${Number((Number(value)*100).toFixed(2))}%`;
const price = value => num(value) === null || Number(value) <= 0 ? '—' : Number(value).toLocaleString('en-US', {maximumFractionDigits:8});
const badge = (label, kind='neutral') => `<span class="badge ${kind}"><i></i>${esc(label)}</span>`;
const statusLabel = {pending:'等待提交',dispatching:'正在提交',queued:'已提交',completed:'CLI 执行完成',unknown:'结果待核对',expired:'已过期',dry_run:'模拟记录','dry-run':'模拟记录',open:'未成交',partially_filled:'部分成交',filled:'已成交',cancelled:'已撤单',canceled:'已撤单'};
const typeLabel = {below:'进入下方观察区',above:'进入上方观察区',cross_below:'向下穿越',cross_above:'向上穿越',hold_above:'持续站上',hold_below:'持续低于',pct_drop:'窗口急跌',pct_rise:'窗口急涨'};
const healthLabel = {receiving:['行情正常','good'],waiting_trade:['等待新成交','waiting'],state_stale:['状态未更新','bad'],unavailable:['暂不可用','bad']};
let snapshot, stream, retryTimer, receivedAt = 0, currentTab = 'overview';
const cache = new Map();
function setHTML(id, html) { if(cache.get(id) === html) return; const el=$(id);const open=[...el.querySelectorAll('details[open]')].map(n=>n.dataset.key);el.innerHTML=html;el.querySelectorAll('details').forEach(n=>{n.open=open.includes(n.dataset.key);});cache.set(id,html); }
function empty(text='尚无触发记录', extra='条件命中并通过确认后，事件会出现在这里。') {return `<div class="empty"><div class="empty-icon">◷</div><strong>${esc(text)}</strong><p>${esc(extra)}</p></div>`;}
function holdings(p) {
  if(!p) {setHTML('summary','<div class="loading-card">持仓记录暂不可用</div>');setHTML('positions','');setHTML('allocation','');return;}
  const positions=Array.isArray(p.positions)?p.positions:[], orders=Array.isArray(p.orders)?p.orders:[];
  const active=orders.filter(o=>o.side==='buy'&&['open','partially_filled'].includes(o.status));
  const heldKnown=p.allocation_basis==='total_crypto_account'&&positions.length>0&&positions.every(x=>num(x.allocation_fraction)!==null), pendingKnown=active.every(x=>x.status==='open'&&num(x.planned_allocation_fraction)!==null);
  const held=positions.reduce((sum,x)=>sum+(num(x.allocation_fraction)||0),0), pending=active.reduce((sum,x)=>sum+(num(x.planned_allocation_fraction)||0),0);
  const known=heldKnown&&pendingKnown, left=known?Math.max(0,1-held-pending):null;
  const stats=[['已确认持仓',heldKnown?fraction(held):'待确认',heldKnown?`占整个账户 ${percent(held)}`:'等待补充仓位'],['未成交买单计划',pendingKnown?fraction(pending):'待核对',`${active.length} 笔挂单 · 不计入实际持仓`],['未分配计划比例',fraction(left),known&&held+pending>1?'现有持仓与挂单计划已超过账户预算':'按申报比例估算，非实时可用余额']];
  setHTML('summary',stats.map(([label,value,note])=>`<div class="stat-card"><div class="stat-label">${label}<span>↗</span></div><div class="stat-value">${esc(value)}</div><div class="stat-caption">${esc(note)}</div></div>`).join(''));
  setHTML('allocation',`<div class="allocation"><div class="allocation-bar" role="img" aria-label="已确认仓位 ${esc(fraction(held))}，挂单计划 ${esc(fraction(pending))}"><div class="allocation-held" data-width="${Math.min(100,Math.max(0,held*100))}"></div><div class="allocation-pending" data-width="${Math.min(100,Math.max(0,pending*100))}"></div></div><div class="allocation-labels"><span>● 已确认持仓 ${esc(fraction(held))}</span><span>▨ 未成交计划 ${esc(fraction(pending))}</span></div></div>`);
  $('allocation').querySelectorAll('[data-width]').forEach(el=>{el.style.width=el.dataset.width+'%';});
  const rows=positions.map(p=>`<tr><td><span class="position-name">${esc(p.asset)}</span>${badge(Number(p.allocation_fraction)>0?'持仓中':'未建仓',Number(p.allocation_fraction)>0?'good':'neutral')}<small>${esc(p.symbol)} · 现货</small></td><td>${esc(fraction(p.allocation_fraction))}<small>账户 ${esc(percent(p.allocation_fraction))}</small></td><td>${p.average_cost?`${esc(price(p.average_cost))} ${esc(p.cost_currency==='CNY'?'人民币':p.cost_currency)}`:'—'}<small>${p.average_cost?'已确认持仓均价':'尚无成交成本'}</small></td><td>${esc(fmtTime(p.confirmed_at))}<small>${esc(p.note)}</small></td></tr>`);
  rows.push(...orders.map(o=>`<tr><td><span class="position-name">${esc(o.asset)}</span>${badge(statusLabel[o.status]||o.status,'waiting')}<small>${o.side==='buy'?'限价买单':'限价卖单'} · ${esc(o.symbol)}</small></td><td>${esc(fraction(o.planned_allocation_fraction))}<small>计划比例 · ${esc(percent(o.planned_allocation_fraction))}</small></td><td>${esc(price(o.limit_price))} ${esc(o.quote_currency)}<small>用户报告的挂单价格</small></td><td>${esc(fmtTime(o.confirmed_at))}<small>${esc(o.note)}</small></td></tr>`));
  setHTML('positions',rows.join('')||'<tr><td colspan="4">还没有用户确认的仓位记录。</td></tr>');
  setHTML('updates',(p.updates||[]).slice(-4).reverse().map(u=>`<div class="update">${esc(u.summary)}<time>${esc(fmtTime(u.at))}</time></div>`).join('')||empty('暂无更新','用户在对话中提供信息后，AI 会在此记录。'));
  setHTML('missing',(p.missing_information||[]).length?`尚待补充：${esc(p.missing_information.join('、'))}。给出具体加仓比例前，会先核对可用资金与风险预算。`:'仓位信息按最近一次用户确认显示；不连接交易账户自动读取。');
}
function eventsMarkup(events, compact=false) {
  if(!events.length) return empty();
  return events.map(e=> compact?`<div class="event-mini">${badge(statusLabel[e.status]||e.status,e.status==='unknown'?'bad':'neutral')}${esc(e.symbol)} · ${esc(e.rule_id)}<small>${esc(fmtTime(e.time_utc))} · ${esc(price(e.price))} USDT</small></div>`:`<article class="event-card"><div class="event-content">${badge(statusLabel[e.status]||e.status,e.status==='unknown'?'bad':'neutral')}<strong>${esc(e.symbol)} · ${esc(e.rule_id)}</strong><p>触发价 ${esc(price(e.price))} USDT · 阈值 ${esc(e.threshold)} · ${esc(fmtTime(e.time_utc))}</p><p>事件 ${esc(e.id)}${e.synthetic?' · 合成测试':''}</p></div><details data-key="event-${esc(e.id)}"><summary>查看触发消息与提交结果</summary><pre>${esc(e.message)}\n\n提交结果：${esc(e.result||'尚无结果')}</pre></details></article>`).join('');
}
function render(s) {
  snapshot=s; const monitors=Array.isArray(s.monitors)?s.monitors:[];
  $('last-sync').textContent=fmtTime(s.generated_at);
  $('global-error').hidden=!s.portfolio_error;$('global-error').textContent=s.portfolio_error||'';
  holdings(s.portfolio);
  $('monitor-count').textContent=monitors.length;
  setHTML('markets',monitors.map(m=>{const [label,kind]=healthLabel[m.health]||healthLabel.unavailable;const enabled=m.rules.filter(r=>r.enabled).length;return `<article class="market-card"><div class="market-top"><div class="asset-title"><div class="coin-icon ${m.id==='render'?'render':''}">${esc(m.asset?.includes('RENDER')?'R':m.asset?.includes('牛来')?'牛':m.asset?.[0]||'◈')}</div><div><h3>${esc(m.asset||m.id)}</h3><p>${esc(m.symbol)}</p></div></div>${badge(label,kind)}</div><div class="market-price">${esc(price(m.last_tick?.price))}<small>USDT</small></div><div class="market-meta">最近成交 ${m.last_tick?.time_ms?esc(fmtTime(m.last_tick.time_ms)):'等待行情'} · ${m.last_tick?.source==='binance_ws'?'实时推送':m.last_tick?.source==='binance_rest'?'备用行情':'尚无来源'}</div>${m.error?`<div class="market-warning">${esc(m.error)}</div>`:''}${!m.config_applied?'<div class="market-warning">配置尚未确认生效</div>':''}${m.last_error?`<div class="market-warning">数据源提示：${esc(m.last_error)}</div>`:''}<div class="market-bottom"><span>${enabled} 条规则 · ${m.mode==='live'?'正式监控':m.mode==='dry-run'?'模拟监控':'等待启动'}</span><span>已接收 ${Number(m.accepted_ticks||0).toLocaleString()} 笔</span></div></article>`;}).join(''));
  const allEvents=monitors.flatMap(m=>m.events||[]).sort((a,b)=>b.time_ms-a.time_ms).slice(0,60);
  $('nav-rules').textContent=monitors.reduce((n,m)=>n+m.rules.length,0);$('nav-events').textContent=monitors.reduce((n,m)=>n+m.event_count,0);
  setHTML('recent-events',eventsMarkup(allEvents.slice(0,3),true));
  if(currentTab==='events')setHTML('event-list',eventsMarkup(allEvents));
  if(currentTab==='rules')setHTML('rule-list',monitors.map(m=>`<section class="rule-group"><h2>${esc(m.asset||m.id)} <span class="muted">${esc(m.symbol)} · 每 UTC 日最多 ${m.budget_limit} 次提交</span></h2>${m.rules.map(r=>`<article class="rule-card"><div class="rule-head">${badge(r.enabled?'已启用':'已停用',r.enabled?'good':'neutral')}<strong>${esc(r.id)}</strong><span class="muted">${esc(typeLabel[r.type]||r.type)}</span><span class="rule-price">${esc(r.threshold)} <small>${r.type.startsWith('pct_')?'%':'USDT'}</small></span></div><div class="rule-meta"><span>连续确认 ${r.confirm_ticks} 次${r.confirm_for!=='0s'?' / '+esc(r.confirm_for):''}</span><span>冷却 ${esc(r.cooldown)}</span><span>迟滞 ${esc(r.hysteresis)}${r.type.startsWith('pct_')?' 个百分点':' USDT'}</span>${r.window!=='0s'?`<span>窗口 ${esc(r.window)}</span>`:''}<span>累计触发 ${Number(r.state?.fire_count||0)} 次</span></div><div class="rule-reason">${esc(r.message_template)}</div><details data-key="rule-${esc(m.id)}-${esc(r.id)}"><summary>查看发送给 AI 的完整消息模板</summary><pre>${esc(r.full_message_template)}</pre></details></article>`).join('')}</section>`).join(''));
}
function markStale(){if(snapshot)render({...snapshot,monitors:snapshot.monitors.map(m=>({...m,health:'state_stale'}))});}
function connection(label, kind){$('connection').className=`badge ${kind}`;$('connection').innerHTML=`<i></i>${esc(label)}`;}
function connect(){if(document.hidden||stream)return;clearTimeout(retryTimer);stream=new EventSource('/api/events');stream.addEventListener('snapshot',event=>{try{const s=JSON.parse(event.data);receivedAt=Date.now();connection('实时同步中','good');render(s);}catch{connection('记录解析失败','bad');}});stream.onerror=()=>{markStale();connection('连接中断，正在重连','waiting');stream?.close();stream=null;clearTimeout(retryTimer);retryTimer=setTimeout(connect,5000+Math.random()*2000);};}
document.addEventListener('visibilitychange',()=>{if(document.hidden){clearTimeout(retryTimer);stream?.close();stream=null;connection('页面已暂停刷新','neutral');}else{connection('正在恢复同步','waiting');connect();}});
document.querySelectorAll('[data-tab]').forEach(button=>button.addEventListener('click',()=>{currentTab=button.dataset.tab;document.querySelectorAll('.view').forEach(v=>v.hidden=v.id!==currentTab);document.querySelectorAll('.nav').forEach(n=>n.classList.toggle('active',n.dataset.tab===currentTab));$('page-title').textContent={overview:'现货监控',rules:'监控规则',events:'触发记录'}[currentTab];$('page-subtitle').textContent={overview:'行情持续观察，仓位以你的成交确认为准。',rules:'查看每一个阈值，以及条件命中后 AI 会收到什么。',events:'从价格事件到消息提交，每一步都有记录。'}[currentTab];if(snapshot)render(snapshot);}));
setInterval(()=>{if(!document.hidden&&receivedAt&&Date.now()-receivedAt>6000){markStale();connection('同步延迟，显示上次记录','waiting');}},2000);
connect();
