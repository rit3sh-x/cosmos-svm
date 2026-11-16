// Shared by every suite: the chain's endpoints and payer, a case runner that
// names each case, and a wait on a condition with a deadline, not on a clock.
import fs from "node:fs";
import { Keypair } from "@solana/web3.js";

export const RPC = process.env.SVM_RPC ?? "http://127.0.0.1:8899";
export const WS = process.env.SVM_WS ?? "ws://127.0.0.1:8900";
export const payer = Keypair.fromSecretKey(Uint8Array.from(JSON.parse(fs.readFileSync(process.env.SVM_PAYER, "utf8"))));
export const mint = process.env.SVM_MINT;

let failed = 0;

export async function check(name, fn) {
  try {
    const detail = await fn();
    console.log(`ok      ${name}${detail ? ": " + detail : ""}`);
  } catch (e) {
    failed++;
    console.log(`FAIL    ${name}: ${e?.stack ?? e}`);
  }
}

export function assert(cond, message) {
  if (!cond) throw new Error(message);
}

// until polls fn every 250 ms until it returns a value that is not null or
// undefined, and fails at the deadline.
export async function until(what, fn, ms = 60_000) {
  const deadline = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v !== null && v !== undefined) return v;
    if (Date.now() > deadline) throw new Error(`${what} did not happen within ${ms} ms`);
    await new Promise((r) => setTimeout(r, 250));
  }
}

export function done() {
  console.log(failed === 0 ? "PASS" : `${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
}
