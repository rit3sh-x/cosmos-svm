// Opens the Solana Explorer at commit aa1b89bf5d5cf483a3d355e094a7a2aad4c85828
// in a headless browser against this chain. The Explorer is built unmodified
// and served from its own origin. Mode "pages" opens every traced page and
// fails on an error card. Mode "refused" runs against a chain whose CORS list
// omits the Explorer's origin. There the transaction page must show that the
// cluster failed.
import fs from "node:fs";
import { chromium } from "playwright";
import { RPC, assert, check, done, until } from "./common.mjs";

const EXPLORER = process.env.SVM_EXPLORER ?? "http://localhost:3000";
const OUT = process.env.SVM_EXPLORER_OUT ?? ".";
const mode = process.argv[2] ?? "pages";
// The Explorer's consent-free custom URL.
const cluster = "cluster=custom&customUrl=" + encodeURIComponent("http://localhost:8899");
// The texts the traced error cards show.
const errorCards = /There was a problem|Failed to fetch|Unable to fetch|RPC did not return|RPC is not responding|Something went wrong/;

const browser = await chromium.launch();
const page = await browser.newPage();

// ready is a selector string, or a pattern for text that the loaded page
// shows. The Explorer renders some text twice, with one copy hidden at each
// breakpoint. Only a visible match counts.
async function open(name, path, ready, refused = false) {
  await check(`explorer ${name}`, async () => {
    const sep = path.includes("?") ? "&" : "?";
    await page.goto(`${EXPLORER}${path}${sep}${cluster}`, { waitUntil: "domcontentloaded" });
    try {
      const target = typeof ready === "string" ? page.locator(ready) : page.getByText(ready);
      await target.filter({ visible: true }).first().waitFor({ timeout: 90_000 });
      await page.waitForLoadState("networkidle", { timeout: 30_000 }).catch(() => {});
      const text = await page.locator("body").innerText();
      const card = text.match(errorCards);
      assert(refused || !card, `an error card: ${card?.[0]}`);
    } finally {
      await page.screenshot({ path: `${OUT}/${name.replaceAll(/\W+/g, "-")}.png`, fullPage: true });
      fs.writeFileSync(`${OUT}/${name.replaceAll(/\W+/g, "-")}.txt`, await page.locator("body").innerText().catch(() => ""));
    }
  });
}

const { SVM_SIGNATURE: sig, SVM_SLOT: slot, SVM_PAYER_ADDRESS: account, SVM_MINT: mint } = process.env;
if (mode === "refused") {
  // ClusterStatus.Failure.
  await open("refused origin", `/tx/${sig}`, /RPC is not responding/, true);
} else {
  // The home page stats stop after 10 s without a performance sample. Agave
  // and svmd take their first sample 60 s after start.
  await until("a performance sample", async () => {
    const res = await fetch(RPC, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "getRecentPerformanceSamples", params: [1] }),
    });
    const { result } = await res.json();
    return result.length > 0 ? result : null;
  }, 120_000);
  await open("home", "/", /Transaction count/i);
  await open("supply", "/supply", /Circulating/i);
  await open("transaction", `/tx/${sig}`, /Success/);
  await open("block", `/block/${slot}`, `a[href*="/tx/${sig}"]`);
  await open("epoch", "/epoch/0", /Epoch/);
  await open("account", `/address/${account}`, /Balance/i);
  await open("history", `/address/${account}`, `a[href*="/tx/${sig}"]`);
  await open("tokens", `/address/${account}/tokens`, `a[href*="/address/${mint}"]`);
  await open("token", `/address/${mint}`, /Supply/i);
}

await browser.close();
done();
