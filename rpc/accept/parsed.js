// Checks @solana/web3.js against the jsonParsed rendering. The client validates
// a parsed body against its own schema and throws on a base64 body. A
// wrong shape from getParsedTokenAccountsByOwner fails here, not as a pass.
const web3 = require('@solana/web3.js');

// The chain under test. It is local unless the four-validator acceptance run
// points it at one of its machines.
const RPC = process.env.SVM_RPC || 'http://127.0.0.1:8899';

(async () => {
  const [owner, mint, ata] = process.argv.slice(2);
  const conn = new web3.Connection(RPC, 'confirmed');

  const held = await conn.getParsedTokenAccountsByOwner(
    new web3.PublicKey(owner), { mint: new web3.PublicKey(mint) });
  const row = held.value.find((v) => v.pubkey.toBase58() === ata);
  if (!row) throw new Error(`${ata} is not in the parsed array`);
  const info = row.account.data.parsed.info;
  if (row.account.data.program !== 'spl-token') {
    throw new Error(`program is ${row.account.data.program}`);
  }
  if (info.mint !== mint || info.owner !== owner) {
    throw new Error(`parsed info names ${info.mint} / ${info.owner}`);
  }
  if (info.tokenAmount.uiAmountString !== '750' || info.tokenAmount.decimals !== 9) {
    throw new Error(`token amount is ${JSON.stringify(info.tokenAmount)}`);
  }
  console.log(`web3.js getParsedTokenAccountsByOwner: ${info.tokenAmount.uiAmountString} of ${mint}`);

  const single = await conn.getParsedAccountInfo(new web3.PublicKey(mint));
  if (single.value.data.program !== 'spl-token' || single.value.data.parsed.type !== 'mint') {
    throw new Error(`the mint rendered as ${JSON.stringify(single.value.data).slice(0, 120)}`);
  }
  console.log(`web3.js getParsedAccountInfo: mint with ${single.value.data.parsed.info.decimals} decimals`);

  // The clock sysvar has a parser. An owner with no parser keeps base64, as in
  // Agave, and this client accepts base64 on the singular method.
  const clock = await conn.getParsedAccountInfo(web3.SYSVAR_CLOCK_PUBKEY);
  if (clock.value.data.parsed.type !== 'clock') {
    throw new Error(`the clock sysvar rendered as ${JSON.stringify(clock.value.data).slice(0, 120)}`);
  }
  console.log(`web3.js getParsedAccountInfo: clock at slot ${clock.value.data.parsed.info.slot}`);
  process.exit(0);
})().catch((e) => { console.error('FAIL', e.message); process.exit(1); });
