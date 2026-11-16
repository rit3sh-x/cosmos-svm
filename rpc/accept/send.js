// Runs @solana/web3.js sendAndConfirmTransaction at one commitment level. The
// call returns only if the websocket answers signatureSubscribe.
const web3 = require('@solana/web3.js');

// The chain under test. It is local unless the four-validator acceptance run
// points it at one of its machines.
const RPC = process.env.SVM_RPC || 'http://127.0.0.1:8899';
const WS = process.env.SVM_WS || 'ws://127.0.0.1:8900';
const fs = require('fs');

(async () => {
  const [commitment, payerFile] = process.argv.slice(2);
  const conn = new web3.Connection(RPC, { commitment, wsEndpoint: WS });
  const from = web3.Keypair.fromSecretKey(Uint8Array.from(JSON.parse(fs.readFileSync(payerFile))));
  const to = web3.Keypair.generate();
  const tx = new web3.Transaction().add(web3.SystemProgram.transfer({
    fromPubkey: from.publicKey, toPubkey: to.publicKey, lamports: 2_000_000,
  }));
  const sig = await web3.sendAndConfirmTransaction(conn, tx, [from], { commitment });
  const status = (await conn.getSignatureStatus(sig)).value.confirmationStatus;
  if (status !== 'finalized') throw new Error(`asked ${commitment}, got ${status}`);
  console.log(`${commitment}: ${sig} ${status}`);
  process.exit(0);
})().catch((e) => { console.error('FAIL', e.message); process.exit(1); });
