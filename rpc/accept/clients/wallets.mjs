// The two wallet call sequences that web3.js 1.99.0 sends to this chain, as the dApp and as Backpack.
import {
  Connection, Keypair, SystemProgram, Transaction, TransactionMessage, VersionedTransaction,
} from "@solana/web3.js";
import { RPC, assert, check, done, payer, until } from "./common.mjs";

const connection = new Connection(RPC, "confirmed");
const transfer = () => SystemProgram.transfer({ fromPubkey: payer.publicKey, toPubkey: Keypair.generate().publicKey, lamports: 1_000_000 });

await check("wallet-standard fallback: a test signer signs, the dApp's connection sends and confirms", async () => {
  const { blockhash, lastValidBlockHeight } = await connection.getLatestBlockhash();
  const tx = new Transaction({ feePayer: payer.publicKey, blockhash, lastValidBlockHeight }).add(transfer());
  tx.sign(payer);
  const signature = await connection.sendRawTransaction(tx.serialize());
  const { value } = await connection.confirmTransaction({ signature, blockhash, lastValidBlockHeight }, "confirmed");
  assert(value.err === null, JSON.stringify(value));
  return signature;
});

await check("Backpack: blockhash, simulate, send, poll getParsedTransaction", async () => {
  const { blockhash } = await connection.getLatestBlockhash();
  const message = new TransactionMessage({ payerKey: payer.publicKey, recentBlockhash: blockhash, instructions: [transfer()] }).compileToV0Message();
  const tx = new VersionedTransaction(message);
  tx.sign([payer]);
  const simulated = await connection.simulateTransaction(tx, { sigVerify: true });
  assert(simulated.value.err === null, JSON.stringify(simulated.value));
  const signature = await connection.sendRawTransaction(tx.serialize(), { skipPreflight: true });
  const parsed = await until("getParsedTransaction", () =>
    connection.getParsedTransaction(signature, { commitment: "confirmed", maxSupportedTransactionVersion: 0 }));
  assert(parsed.meta.err === null && parsed.transaction.message.instructions[0].parsed.type === "transfer", JSON.stringify(parsed));
  return signature;
});

done();
