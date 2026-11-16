// Checks the websocket limits and silences with two clients: a raw socket for
// the frames that web3.js hides, and web3.js for the idle minute that its
// heartbeat must survive.
const web3 = require('@solana/web3.js');
const WebSocket = require('ws');

// The chain under test. It is local unless the four-validator acceptance run
// points it at one of its machines.
const RPC = process.env.SVM_RPC || 'http://127.0.0.1:8899';
const URL = process.env.SVM_WS || 'ws://127.0.0.1:8900';

const open = () => new Promise((ok, bad) => {
  const socket = new WebSocket(URL);
  socket.on('open', () => ok(socket));
  socket.on('error', bad);
});

const quiet = (socket, ms) => new Promise((ok, bad) => {
  const seen = (data) => bad(new Error(`answered a notification with ${data}`));
  socket.on('message', seen);
  setTimeout(() => { socket.off('message', seen); ok(); }, ms).unref();
});

const answer = (socket) => new Promise((ok) => socket.once('message', (data) => ok(JSON.parse(data))));

// The server never answers a request with no id, not even with Method not
// found. web3.js sends one every five seconds.
async function pingIsUnanswered() {
  const socket = await open();
  socket.send(JSON.stringify({ jsonrpc: '2.0', method: 'ping' }));
  socket.send(JSON.stringify({ jsonrpc: '2.0', method: 'accountUnsubscribe', params: [1] }));
  await quiet(socket, 3_000);
  socket.close();
  console.log('web3.js ping: unanswered on both listeners');
}

// The server refuses the 1,025th subscription on one connection with Agave's
// string. It refuses an unsubscribe for an id that the connection does not
// hold with Agave's other string. Each subscription asks for its own account,
// because the same params twice on one connection are one subscription, as on
// Agave.
async function theCapIsAProduct() {
  const socket = await open();
  const ask = (method, params) => {
    socket.send(JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }));
    return answer(socket);
  };
  const account = (i) => {
    const key = new Uint8Array(32);
    new DataView(key.buffer).setUint32(0, i + 1, true);
    return new web3.PublicKey(key).toBase58();
  };
  let held = 0;
  for (let i = 0; i < 1024; i += 1) {
    const out = await ask('accountSubscribe', [account(i)]);
    if (out.error) throw new Error(`subscription ${i + 1} was refused: ${JSON.stringify(out.error)}`);
    held += 1;
  }
  const over = await ask('accountSubscribe', [account(1024)]);
  const want = 'Internal Error: Subscription refused. Node subscription limit reached';
  if (!over.error || over.error.code !== -32603 || over.error.message !== want) {
    throw new Error(`the ${held + 1}th subscribe was answered ${JSON.stringify(over)}`);
  }
  const unheld = await ask('slotUnsubscribe', [10_000_000]);
  if (!unheld.error || unheld.error.code !== -32602 || unheld.error.message !== 'Invalid subscription id.') {
    throw new Error(`an id nobody holds was answered ${JSON.stringify(unheld)}`);
  }
  socket.close();
  console.log(`websocket cap: ${held} held, the next refused, an unheld id -32602`);
}

// A connection that sends nothing for a minute survives, because the server
// pings and the client library answers.
async function anIdleMinuteSurvives() {
  const conn = new web3.Connection(RPC, {
    commitment: 'confirmed', wsEndpoint: URL,
  });
  let slots = 0;
  const id = conn.onSlotChange(() => { slots += 1; });
  await new Promise((ok) => setTimeout(ok, 5_000));
  if (slots === 0) throw new Error('no slot notification before the idle hold');
  const before = slots;
  await new Promise((ok) => setTimeout(ok, 65_000));
  if (slots <= before) throw new Error(`the connection went quiet after ${before} slots`);
  await conn.removeSlotChangeListener(id);
  console.log(`web3.js idle minute: ${slots - before} slots after 65 s of sending nothing`);
}

(async () => {
  await pingIsUnanswered();
  const idle = anIdleMinuteSurvives();
  await theCapIsAProduct();
  await idle;
  process.exit(0);
})().catch((e) => { console.error('FAIL', e.message); process.exit(1); });
