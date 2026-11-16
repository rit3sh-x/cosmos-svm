// Anchor's two TypeScript clients. A failing instruction must give a
// SendTransactionError that carries its logs.
import * as coral from "@coral-xyz/anchor";
import * as core from "@anchor-lang/core";
import { Connection, Keypair, SystemProgram, Transaction } from "@solana/web3.js";
import { RPC, assert, check, done, payer } from "./common.mjs";

const connection = new Connection(RPC, "confirmed");

for (const [name, anchor] of [["@coral-xyz/anchor 0.32.1", coral], ["@anchor-lang/core 1.2.1", core]]) {
  await check(`${name}: a failing transfer is a SendTransactionError with its logs`, async () => {
    const provider = new anchor.AnchorProvider(connection, new anchor.Wallet(payer), { commitment: "confirmed" });
    const tx = new Transaction().add(SystemProgram.transfer({
      fromPubkey: payer.publicKey, toPubkey: Keypair.generate().publicKey, lamports: 2n ** 62n,
    }));
    let err;
    try {
      await provider.sendAndConfirm(tx);
    } catch (e) {
      err = e;
    }
    assert(err, "the transfer landed");
    assert(err.constructor.name === "SendTransactionError", `${err.constructor.name}: ${err.message}`);
    const logs = err.logs ?? (await err.getLogs(connection));
    assert(logs?.some((l) => l.includes("insufficient lamports")), `logs ${JSON.stringify(logs)}`);
    return logs.length + " log lines";
  });
}

done();
