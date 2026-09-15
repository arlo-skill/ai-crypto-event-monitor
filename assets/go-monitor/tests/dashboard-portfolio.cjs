const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../web/app.js'), 'utf8');
function render(portfolio) {
  const elements = new Map();
  const document = {
    hidden: true,
    addEventListener() {},
    querySelectorAll() { return []; },
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, {innerHTML: '', querySelectorAll() { return []; }});
      return elements.get(id);
    },
  };
  vm.runInNewContext(source + '\nholdings(' + JSON.stringify(portfolio) + ');', {
    document, setInterval() {}, clearTimeout() {},
  });
  return Object.fromEntries([...elements].map(([id, el]) => [id, el.innerHTML]));
}

const base = {allocation_basis: 'total_crypto_account', updates: [], missing_information: []};
const partial = render({...base,
  positions: [{asset: 'Example', position_status: 'holding', allocation_fraction: null}],
  orders: [{side: 'buy', status: 'partially_filled', planned_allocation_fraction: '0.25'}],
});
assert.match(partial.positions, /持仓中/);
assert.doesNotMatch(partial.positions, /未建仓|尚无成交成本/);
assert.match(partial.positions, /成交均价待补充/);
assert.match(partial.orders, /原订单总计划（含已成交）/);
assert.doesNotMatch(partial.positions, /原订单总计划|部分成交/);
assert.match(partial.orders, /部分成交 · 余单待核对/);
assert.match(partial.summary, /已建仓资产/);
assert.match(partial.summary, /1 种/);
assert.match(partial.allocation, /剩余挂单 待核对/);
assert.doesNotMatch(partial.allocation, /allocation-bar|data-width|2.5 成/);

const unknown = render({...base, positions: [{asset: 'Example', allocation_fraction: null}], orders: []});
assert.match(unknown.positions, /仓位待确认/);
assert.doesNotMatch(unknown.positions, /未建仓/);

const known = render({...base,
  positions: [{asset: 'Empty', allocation_fraction: '0'}, {asset: 'Held', allocation_fraction: '0.25'}],
  orders: [{side: 'buy', status: 'open', planned_allocation_fraction: '0.1'}],
});
assert.match(known.positions, /未建仓/);
assert.match(known.positions, /持仓中/);
assert.match(known.allocation, /allocation-bar/);
assert.match(known.allocation, /已确认持仓 2.5 成/);
assert.match(known.allocation, /未成交计划 1 成/);
const quantityOnly = render({...base,
  positions: [{asset: 'Quantity held', quantity: '12.3456', allocation_fraction: null}], orders: [],
});
assert.match(quantityOnly.positions, /持仓中/);
assert.match(quantityOnly.positions, /12.3456 枚/);
assert.doesNotMatch(quantityOnly.positions, /未建仓/);

for (const status of ['filled', 'cancelled', 'canceled']) {
  const closed = render({...base,
    positions: [{asset: 'Held', position_status: 'holding', quantity: '12', allocation_fraction: '0.25'}],
    orders: [{asset: 'Held', side: 'buy', status, planned_allocation_fraction: '0.4'}],
  });
  assert.match(closed.summary, /0 笔/);
  assert.match(closed.summary, /记录中无待成交买单/);
  assert.match(closed.orders, /历史订单/);
  assert.match(closed.orders, /原订单计划（历史）/);
  assert.doesNotMatch(closed.positions, /历史订单|原订单计划/);
  assert.match(closed.allocation, /未成交计划 0 成/);
}
const knownRemainder = render({...base,
  positions: [{asset: 'Held', allocation_fraction: '0.25'}],
  orders: [{side: 'buy', status: 'partially_filled', planned_allocation_fraction: '0.4', remaining_allocation_fraction: '0.1', remaining_order_status: 'open'}],
});
assert.match(knownRemainder.allocation, /未成交计划 1 成/);
assert.doesNotMatch(knownRemainder.allocation, /未成交计划 4 成/);
console.log('PASS: holding quantities, unknown allocations, partial remainders and closed order history');
