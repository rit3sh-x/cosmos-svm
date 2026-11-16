// The Pyth replay of the ER run. Real price updates, signed by Pyth's
// guardians, go to the base chain through Pyth's own client, for both of
// Pyth's Solana id sets. The rollup must follow the feed account.
//
//   node pyth.js select SET COUNT FILE        store the signatures of COUNT recent mainnet updates
//   node pyth.js rebuild SET FILE OUT         rebuild those updates from mainnet into OUT
//   node pyth.js accounts SET OUT             the accounts a base chain copies from mainnet
//   node pyth.js run SET OUT BASE_RPC ER_RPC KEYPAIR   one result line a check, on fd 3
//
// SET is default (rec5EKMG..., HDwcJBJ..., pythWSn...) or pro, the
// "pro-compatible" ids (rec2HHDD..., HDw2E7P8..., pyt2F414...).
//
// Hermes answers price updates only with a Pyth API key. The updates come
// from the transactions that Pyth's own pusher sent on mainnet: the
// guardian-signed VAA from its write_encoded_vaa instructions, and the Merkle
// update from update_price_feed, rebuilt into the format that Hermes serves.
// select picks the first feed with COUNT updates among the push oracle's last
// 1,000 transactions, and stores their signatures in FILE. That way every run
// replays the same data, and the repository holds no Pyth data.
const crypto = require("crypto");
const bs58m = require("bs58");
const fs = require("fs");
const { Connection, Keypair, PublicKey } = require("@solana/web3.js");
const { Wallet } = require("@coral-xyz/anchor");
const { parseAccumulatorUpdateData, parsePriceFeedMessage } = require("@pythnetwork/price-service-sdk");
const {
    PythSolanaReceiver,
    DEFAULT_PUSH_ORACLE_PROGRAM_ID,
    DEFAULT_RECEIVER_PROGRAM_ID,
    DEFAULT_WORMHOLE_PROGRAM_ID,
    PRO_COMPATIBLE_PUSH_ORACLE_PROGRAM_ID,
    PRO_COMPATIBLE_RECEIVER_PROGRAM_ID,
    PRO_COMPATIBLE_WORMHOLE_PROGRAM_ID,
} = require("@pythnetwork/pyth-solana-receiver");
const { sendTransactions } = require("@pythnetwork/solana-utils");

const bs58 = bs58m.default ?? bs58m;
const SETS = {
    default: { receiver: DEFAULT_RECEIVER_PROGRAM_ID, wormhole: DEFAULT_WORMHOLE_PROGRAM_ID, pushOracle: DEFAULT_PUSH_ORACLE_PROGRAM_ID },
    pro: {
        receiver: PRO_COMPATIBLE_RECEIVER_PROGRAM_ID,
        wormhole: PRO_COMPATIBLE_WORMHOLE_PROGRAM_ID,
        pushOracle: PRO_COMPATIBLE_PUSH_ORACLE_PROGRAM_ID,
    },
};
const MAINNET = "https://api.mainnet-beta.solana.com";
// Mainnet's fee is 0, so nothing moves to a treasury and none is copied.
const TREASURY_ID = 0;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const pda = (seeds, program) => PublicKey.findProgramAddressSync(seeds, program)[0];
const discriminator = (name) => crypto.createHash("sha256").update(`global:${name}`).digest().subarray(0, 8);
const u16be = (n) => Buffer.from([n >> 8, n & 0xff]);
const message = (update) => parsePriceFeedMessage(parseAccumulatorUpdateData(Buffer.from(update, "base64")).updates[0].message);

// call sends a JSON-RPC request to mainnet and waits out its rate limit.
async function call(method, params) {
    for (let wait = 500; ; wait = 5000) {
        await sleep(wait);
        const r = await fetch(MAINNET, {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
        });
        if (r.status === 429) continue;
        const { result, error } = await r.json();
        if (error?.code === 429) continue;
        if (error) throw new Error(`${method}: ${error.message}`);
        return result;
    }
}

// instructions reads a mainnet transaction, inner instructions included,
// because the pusher sends some updates through a task program. Some are V1
// transactions, which web3.js 1.99 cannot parse, so it reads the JSON encoding.
async function instructions(signature) {
    const tx = await call("getTransaction", [signature, { encoding: "json", maxSupportedTransactionVersion: 1 }]);
    if (!tx) throw new Error(`mainnet has no transaction ${signature}`);
    const loaded = tx.meta.loadedAddresses ?? {};
    const keys = [...tx.transaction.message.accountKeys, ...(loaded.writable ?? []), ...(loaded.readonly ?? [])].map((k) => new PublicKey(k));
    const inner = (tx.meta.innerInstructions ?? []).flatMap((i) => i.instructions);
    return [...tx.transaction.message.instructions, ...inner].map((ix) => ({
        program: keys[ix.programIdIndex],
        accounts: ix.accounts.map((i) => keys[i]),
        data: Buffer.from(bs58.decode(ix.data)),
    }));
}

// pushes returns the updates of one push. Each update_price_feed instruction
// is PostUpdateParams { message: bytes, proof: Vec<[u8; 20]>, treasury_id },
// and its third account is the encoded VAA that the update was verified
// against.
const pushes = (set, ixs) =>
    ixs
        .filter((ix) => ix.program.equals(SETS[set].pushOracle) && ix.data.subarray(0, 8).equals(discriminator("update_price_feed")))
        .map((ix) => {
            const len = ix.data.readUInt32LE(8);
            const proofs = ix.data.readUInt32LE(12 + len);
            return { vaa: ix.accounts[2], msg: ix.data.subarray(12, 12 + len), proofs, proof: ix.data.subarray(16 + len, 16 + len + 20 * proofs) };
        });

// chunks returns the VAA chunks of a set of transactions: WriteEncodedVaaArgs { index: u32, data: bytes }.
async function chunks(set, signatures) {
    const out = [];
    for (const s of signatures) {
        for (const ix of await instructions(s)) {
            if (ix.program.equals(SETS[set].wormhole) && ix.data.subarray(0, 8).equals(discriminator("write_encoded_vaa"))) {
                out.push({ account: ix.accounts[1].toBase58(), index: ix.data.readUInt32LE(8), data: ix.data.subarray(16, 16 + ix.data.readUInt32LE(12)) });
            }
        }
    }
    return out;
}

function accumulator(vaaChunks, p) {
    const signed = Buffer.alloc(Math.max(...vaaChunks.map((c) => c.index + c.data.length)));
    for (const c of vaaChunks) c.data.copy(signed, c.index);
    return Buffer.concat([
        Buffer.from("504e415501000000", "hex"),
        u16be(signed.length),
        signed,
        Buffer.from([1]),
        u16be(p.msg.length),
        p.msg,
        Buffer.from([p.proofs]),
        p.proof,
    ]).toString("base64");
}

// vaaOf finds the transactions that wrote an encoded VAA account, and its
// chunks.
async function vaaOf(set, account, cache) {
    if (!cache.has(account)) {
        const signatures = [];
        let found = [];
        for (const w of await call("getSignaturesForAddress", [account, { limit: 20 }])) {
            const c = (await chunks(set, [w.signature])).filter((c) => c.account === account);
            if (c.length) {
                signatures.push(w.signature);
                found = found.concat(c);
            }
        }
        cache.set(account, { signatures, chunks: found });
    }
    return cache.get(account);
}

async function select(set, count, file) {
    const byFeed = new Map();
    for (const { signature, err } of await call("getSignaturesForAddress", [SETS[set].pushOracle.toBase58(), { limit: 1000 }])) {
        if (err) continue;
        for (const p of pushes(set, await instructions(signature))) {
            const id = "0x" + parsePriceFeedMessage(p.msg).feedId.toString("hex");
            const list = [...(byFeed.get(id) ?? []), { push: signature, account: p.vaa.toBase58() }];
            byFeed.set(id, list);
            if (list.length < Number(count)) continue;
            const cache = new Map();
            const updates = [];
            for (const u of list.reverse()) updates.push({ push: u.push, vaa: (await vaaOf(set, u.account, cache)).signatures });
            const stored = fs.existsSync(file) ? JSON.parse(fs.readFileSync(file)) : {};
            stored[set] = { feed: id, updates };
            fs.writeFileSync(file, JSON.stringify(stored, null, 2) + "\n");
            console.error(`select ${set}: feed ${id}, ${count} updates`);
            return;
        }
    }
    throw new Error(`no feed has ${count} updates in the push oracle's last 1000 transactions`);
}

// rebuild writes the stored updates, oldest first, in the format that Hermes
// serves. One push can update several feeds from one VAA, so rebuild takes the
// update of the stored feed.
async function rebuild(set, file, out) {
    const { feed, updates } = JSON.parse(fs.readFileSync(file))[set];
    const rebuilt = [];
    for (const u of updates) {
        const vaa = await chunks(set, u.vaa);
        const found = pushes(set, await instructions(u.push))
            .filter((p) => vaa.some((c) => c.account === p.vaa.toBase58()))
            .map((p) => accumulator(vaa.filter((c) => c.account === p.vaa.toBase58()), p))
            .find((update) => "0x" + message(update).feedId.toString("hex") === feed);
        if (!found) throw new Error(`${u.push}: no update of feed ${feed}`);
        rebuilt.push(found);
    }
    fs.writeFileSync(out, JSON.stringify(rebuilt));
}

// accounts prints the receiver's config, the Wormhole receiver's config and
// the guardian set that signed the updates.
function accounts(set, file) {
    const { receiver, wormhole } = SETS[set];
    const vaa = parseAccumulatorUpdateData(Buffer.from(JSON.parse(fs.readFileSync(file))[0], "base64")).vaa;
    const index = Buffer.alloc(4);
    index.writeUInt32BE(vaa.readUInt32BE(1));
    console.log(pda([Buffer.from("config")], receiver).toBase58());
    console.log(pda([Buffer.from("Bridge")], wormhole).toBase58());
    console.log(pda([Buffer.from("GuardianSet"), index], wormhole).toBase58());
}

// send builds and sends the transactions that add asks for, and returns the
// units that each one used.
async function send(receiver, add) {
    const builder = receiver.newTransactionBuilder({ closeUpdateAccounts: false });
    await add(builder);
    const txs = await builder.buildVersionedTransactions({ computeUnitPriceMicroLamports: 0 });
    const units = [];
    for (const sig of await sendTransactions(txs, receiver.connection, receiver.wallet)) {
        const tx = await receiver.connection.getTransaction(sig, { commitment: "confirmed", maxSupportedTransactionVersion: 0 });
        units.push(tx.meta.computeUnitsConsumed);
    }
    return { builder, units: units.join(" ") };
}

// check runs one case and writes its result line to fd 3, because Pyth's
// client prints its retries on stdout. A case returns its value, or throws.
async function check(test, f) {
    let r;
    try {
        const value = await f();
        r = { test, ok: true, ...(value === undefined ? {} : { value: String(value) }) };
    } catch (e) {
        r = { test, ok: false, error: `${e.message}\n${e.logs ? e.logs.join("\n") : ""}` };
    }
    fs.writeSync(3, JSON.stringify(r) + "\n");
}

async function run(set, file, baseUrl, erUrl, keypair) {
    const { receiver: receiverProgramId, wormhole: wormholeProgramId, pushOracle: pushOracleProgramId } = SETS[set];
    const updates = JSON.parse(fs.readFileSync(file));
    const id = "0x" + message(updates[0]).feedId.toString("hex");
    const payer = Keypair.fromSecretKey(Uint8Array.from(JSON.parse(fs.readFileSync(keypair))));
    const base = new Connection(baseUrl, "confirmed");
    const er = new Connection(erUrl, "confirmed");
    const receiver = new PythSolanaReceiver({ connection: base, wallet: new Wallet(payer), treasuryId: TREASURY_ID, receiverProgramId, wormholeProgramId, pushOracleProgramId });
    const feed = receiver.getPriceFeedAccountAddress(0, id);

    await check("an update posts into a PriceUpdateV2 account, fully verified", async () => {
        const { builder, units } = await send(receiver, (b) => b.addPostPriceUpdates([updates[0]]));
        const posted = await receiver.fetchPriceUpdateAccount(builder.getPriceUpdateAccount(id));
        if (!posted.verificationLevel.full) throw new Error(`verification ${JSON.stringify(posted.verificationLevel)}`);
        return units;
    });
    await check("a copy with one Merkle proof byte flipped is refused", async () => {
        const tampered = Buffer.from(updates[0], "base64");
        tampered[tampered.length - 1] ^= 1;
        try {
            await send(receiver, (b) => b.addPostPriceUpdates([tampered.toString("base64")]));
        } catch (e) {
            const code = /"Custom":(\d+)/.exec(e.message);
            if (code) return code[1];
            throw e;
        }
        throw new Error("the tampered update landed");
    });
    await check("an update writes the push oracle's shard 0 feed account", async () => (await send(receiver, (b) => b.addUpdatePriceFeed([updates[0]], 0))).units);
    await check("the rollup's copy of the feed account is the base chain's", async () => {
        const [clone, now] = [await er.getAccountInfo(feed), await base.getAccountInfo(feed)];
        if (!clone) throw new Error("the rollup has no account at the feed address");
        if (!clone.data.equals(now.data)) throw new Error("the rollup's bytes differ from the base chain's");
    });
    await check("the rollup takes each newer push", async () => {
        for (const update of updates.slice(1)) {
            await send(receiver, (b) => b.addUpdatePriceFeed([update], 0));
            const want = (await base.getAccountInfo(feed)).data;
            const deadline = Date.now() + 30000;
            for (;;) {
                const got = await er.getAccountInfo(feed);
                if (got && got.data.equals(want)) break;
                if (Date.now() > deadline) throw new Error(`publish_time ${message(update).publishTime}: the rollup is still old after 30 s`);
                await sleep(25);
            }
        }
        return updates.length - 1;
    });
}

const [mode, ...args] = process.argv.slice(2);
Promise.resolve({ select, rebuild, accounts, run }[mode](...args)).then(
    () => process.exit(0),
    (e) => {
        console.error(e);
        process.exit(1);
    },
);
