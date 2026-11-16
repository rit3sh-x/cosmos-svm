// Checks web3.js 1.99.0 and kit 8.4.0: the send, subscribe and parsed paths,
// the four transaction and block calls, and getEpochInfo in both clients.
import {
  Connection, Keypair, LAMPORTS_PER_SOL, PublicKey, SystemProgram, Transaction, sendAndConfirmTransaction,
} from "@solana/web3.js";
import {
  address, appendTransactionMessageInstruction, createKeyPairSignerFromBytes, createSolanaRpc,
  createSolanaRpcSubscriptions, createTransactionMessage, getSignatureFromTransaction, lamports, pipe,
  sendAndConfirmTransactionFactory, setTransactionMessageFeePayerSigner, setTransactionMessageLifetimeUsingBlockhash,
  signTransactionMessageWithSigners,
} from "@solana/kit";
import { getTransferSolInstruction } from "@solana-program/system";
import { RPC, WS, assert, check, done, mint, payer, until } from "./common.mjs";

const connection = new Connection(RPC, { commitment: "confirmed", wsEndpoint: WS });
const recipient = Keypair.generate().publicKey;
let signature, slot;

await check("web3.js subscribe: onAccountChange sees the transfer land", async () => {
  let seen = null;
  const id = connection.onAccountChange(recipient, (info) => { seen = info.lamports; }, "confirmed");
  const tx = new Transaction().add(SystemProgram.transfer({ fromPubkey: payer.publicKey, toPubkey: recipient, lamports: LAMPORTS_PER_SOL }));
  signature = await sendAndConfirmTransaction(connection, tx, [payer]);
  const lamportsSeen = await until("the account notification", () => seen);
  await connection.removeAccountChangeListener(id);
  assert(lamportsSeen === LAMPORTS_PER_SOL, `notified ${lamportsSeen} lamports`);
  return signature;
});

await check("web3.js onLogs names the transfer", async () => {
  let logs = null;
  const id = connection.onLogs(payer.publicKey, (l) => { logs = l; }, "confirmed");
  const tx = new Transaction().add(SystemProgram.transfer({ fromPubkey: payer.publicKey, toPubkey: recipient, lamports: 1000 }));
  const sig = await sendAndConfirmTransaction(connection, tx, [payer]);
  const got = await until("the logs notification", () => (logs?.signature === sig ? logs : null));
  await connection.removeOnLogsListener(id);
  assert(got.logs.some((l) => l.includes("11111111111111111111111111111111 success")), got.logs.join("|"));
});

await check("web3.js getTransaction renders", async () => {
  const tx = await connection.getTransaction(signature, { maxSupportedTransactionVersion: 0 });
  assert(tx?.meta?.err === null && tx.transaction.message.staticAccountKeys.length === 3, JSON.stringify(tx));
  slot = tx.slot;
});

await check("web3.js getParsedTransaction parses the transfer", async () => {
  const tx = await connection.getParsedTransaction(signature, { maxSupportedTransactionVersion: 0 });
  const ix = tx.transaction.message.instructions[0];
  assert(ix.program === "system" && ix.parsed.type === "transfer" && ix.parsed.info.lamports === LAMPORTS_PER_SOL, JSON.stringify(ix));
});

await check("web3.js getParsedTransactions", async () => {
  const txs = await connection.getParsedTransactions([signature], { maxSupportedTransactionVersion: 0 });
  assert(txs.length === 1 && txs[0].meta.preBalances.length === 3, JSON.stringify(txs));
});

await check("web3.js getBlock renders", async () => {
  const block = await connection.getBlock(slot, { maxSupportedTransactionVersion: 0 });
  assert(block.transactions.some((t) => t.transaction.signatures[0] === signature), "the block does not hold the transfer");
});

await check("web3.js getParsedBlock parses", async () => {
  const block = await connection.getParsedBlock(slot, { maxSupportedTransactionVersion: 0 });
  const tx = block.transactions.find((t) => t.transaction.signatures[0] === signature);
  assert(tx.transaction.message.instructions[0].parsed.type === "transfer", JSON.stringify(tx));
});

await check("web3.js getParsedAccountInfo parses the mint", async () => {
  const info = await connection.getParsedAccountInfo(new PublicKey(mint));
  assert(info.value.data.program === "spl-token" && info.value.data.parsed.type === "mint", JSON.stringify(info.value.data));
});

await check("web3.js getEpochInfo validates", async () => {
  const info = await connection.getEpochInfo();
  assert(typeof info.transactionCount === "number" && info.absoluteSlot > 0, JSON.stringify(info));
});

const rpc = createSolanaRpc(RPC);
const rpcSubscriptions = createSolanaRpcSubscriptions(WS);

await check("kit getEpochInfo validates", async () => {
  const info = await rpc.getEpochInfo().send();
  assert(info.absoluteSlot > 0n && typeof info.transactionCount === "bigint", String(info.transactionCount));
});

await check("kit sends and confirms a transfer", async () => {
  const signer = await createKeyPairSignerFromBytes(payer.secretKey);
  const { value: blockhash } = await rpc.getLatestBlockhash().send();
  const message = pipe(
    createTransactionMessage({ version: 0 }),
    (m) => setTransactionMessageFeePayerSigner(signer, m),
    (m) => setTransactionMessageLifetimeUsingBlockhash(blockhash, m),
    (m) => appendTransactionMessageInstruction(
      getTransferSolInstruction({ source: signer, destination: address(recipient.toBase58()), amount: lamports(5000n) }), m),
  );
  const tx = await signTransactionMessageWithSigners(message);
  await sendAndConfirmTransactionFactory({ rpc, rpcSubscriptions })(tx, { commitment: "confirmed" });
  return getSignatureFromTransaction(tx);
});

await check("kit getTransaction jsonParsed renders", async () => {
  const tx = await rpc.getTransaction(signature, { encoding: "jsonParsed", maxSupportedTransactionVersion: 0 }).send();
  assert(tx.transaction.message.instructions[0].parsed.type === "transfer", JSON.stringify(tx, (_, v) => typeof v === "bigint" ? String(v) : v));
});

await check("kit getBlock renders", async () => {
  const block = await rpc.getBlock(BigInt(slot), { maxSupportedTransactionVersion: 0, transactionDetails: "signatures" }).send();
  assert(block.signatures.includes(signature), "the block does not name the transfer");
});

done();
