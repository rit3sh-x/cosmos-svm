// Checks @solana/web3.js against the four subscriptions. The client decodes
// each notification with its own schema. An answer with a shape that is
// not mainnet's fails here, not as a pass.
const web3 = require('@solana/web3.js');

// The chain under test. It is local unless the four-validator acceptance run
// points it at one of its machines.
const RPC = process.env.SVM_RPC || 'http://127.0.0.1:8899';
const WS = process.env.SVM_WS || 'ws://127.0.0.1:8900';
const fs = require('fs');

const deadline = (what, ms) => new Promise((_, bad) =>
  setTimeout(() => bad(new Error(`no ${what} within ${ms} ms`)), ms).unref());

const first = (what, subscribe, ms = 60_000) => {
  let id;
  const got = new Promise((ok) => { id = subscribe(ok); });
  return { id: Promise.resolve(id), value: Promise.race([got, deadline(what, ms)]) };
};

(async () => {
  const [payerFile] = process.argv.slice(2);
  const conn = new web3.Connection(RPC, {
    commitment: 'confirmed', wsEndpoint: WS,
  });
  const from = web3.Keypair.fromSecretKey(Uint8Array.from(JSON.parse(fs.readFileSync(payerFile))));
  const to = web3.Keypair.generate();
  const lamports = 3_000_000;

  // Every call uses commitment 'confirmed'. This chain has no confirmed level,
  // but it accepts the value.
  const account = first('accountNotification', (ok) =>
    conn.onAccountChange(to.publicKey, (info, ctx) => ok({ info, ctx }), 'confirmed'));
  const program = first('programNotification', (ok) =>
    conn.onProgramAccountChange(web3.SystemProgram.programId, (keyed, ctx) => {
      if (keyed.accountId.equals(to.publicKey)) ok({ keyed, ctx });
    }, 'confirmed'));
  const logs = first('logsNotification', (ok) =>
    conn.onLogs(to.publicKey, (log, ctx) => ok({ log, ctx }), 'confirmed'));
  const slot = first('slotNotification', (ok) => conn.onSlotChange(ok));

  const tx = new web3.Transaction().add(web3.SystemProgram.transfer({
    fromPubkey: from.publicKey, toPubkey: to.publicKey, lamports,
  }));
  const sig = await web3.sendAndConfirmTransaction(conn, tx, [from], { commitment: 'confirmed' });

  const a = await account.value;
  if (a.info.lamports !== lamports) throw new Error(`account notification carries ${a.info.lamports}`);
  if (!a.info.owner.equals(web3.SystemProgram.programId)) throw new Error('account notification owner');
  if (a.ctx.slot < 1) throw new Error('account notification slot');

  const p = await program.value;
  if (p.keyed.accountInfo.lamports !== lamports) throw new Error('program notification lamports');

  const l = await logs.value;
  if (l.log.signature !== sig) throw new Error(`log notification names ${l.log.signature}`);
  if (l.log.err !== null || l.log.logs.length === 0) throw new Error('log notification body');

  const s = await slot.value;
  if (!(s.slot > 0) || s.parent !== s.slot - 1 || s.root !== s.slot) {
    throw new Error(`slot notification ${JSON.stringify(s)}`);
  }
  console.log(`web3.js subscriptions: account ${a.info.lamports}, program ${p.keyed.accountId.toBase58().slice(0, 8)}, logs ${l.log.logs.length} lines, slot ${s.slot}`);

  // Every unsubscribe removes what it names.
  await conn.removeAccountChangeListener(await account.id);
  await conn.removeProgramAccountChangeListener(await program.id);
  await conn.removeOnLogsListener(await logs.id);
  await conn.removeSlotChangeListener(await slot.id);
  let after = 0;
  conn._rpcWebSocket.on('accountNotification', () => { after += 1; });
  conn._rpcWebSocket.on('slotNotification', () => { after += 1; });
  await new Promise((ok) => setTimeout(ok, 5_000));
  if (after !== 0) throw new Error(`${after} notifications after unsubscribe`);
  console.log('web3.js unsubscribe: silent');
  process.exit(0);
})().catch((e) => { console.error('FAIL', e.message); process.exit(1); });
