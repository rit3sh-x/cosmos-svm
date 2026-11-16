// The steps of the ER run beside the apps: the accounts in an operator's
// genesis, VRF's queues as its oracle opens them, and the counter's state at
// the end.
//
//   node run.js accounts IDENTITY DIR        write the genesis accounts, print "ADDRESS FILE" each
//   node run.js queues BASE_RPC              open VRF's two queues, delegate the rollup's; result lines on fd 3
//   node run.js counter BASE_RPC ER_RPC APP  the counter's state at the end; a result line on fd 3
//   node run.js unlisted BASE_RPC PAYER      a key with no fees vault acting as a validator; a result line on fd 3
//   node run.js secret KEYPAIR               the keypair file's 64 bytes in base58
//   node run.js oracle                       the built-in oracle as svmd init --vrf-oracle takes it
//
// The script writes the accounts directly, as the instructions that make them
// leave them. Only the program's admin can send those instructions, and a
// genesis program has no admin. The accounts are:
//   - the delegation program's fees vaults for the validator IDENTITY and for
//     the protocol (InitValidatorFeesVault and InitProtocolFeesVault)
//   - VRF's oracle list, with the oracle and its data (ModifyOracles),
//     registered at slot 0
// The lamports are Agave's rent-exempt minimum, which is above this chain's
// minimum, so one file serves both chains.
const crypto = require("crypto");
const fs = require("fs");
const path = require("path");
const bs58m = require("bs58");
const { RistrettoPoint } = require("@noble/curves/ed25519");
const { Connection, Keypair, PublicKey, SystemProgram, Transaction, TransactionInstruction, sendAndConfirmTransaction } = require("@solana/web3.js");

const bs58 = bs58m.default ?? bs58m;
const DLP = new PublicKey("DELeGGvXpWV2fqJUhqcF5ZSYMS4JTLjteaAMARRSaeSh");
const VRF = new PublicKey("Vrf1RNUjXmQGjmQrQLvJHs9SNkvDJEsRVFPkfSQUwGz");
// The oracle's built-in identity, paywJ..., which owns the local queues of the
// examples. solana-vrf's vrf-oracle holds this key.
const ORACLE = Keypair.fromSecretKey(
    bs58.decode("D4fURjsRpMj1vzfXqHgL94UeJyXR8DFyfyBDmbY647PnpuDzszvbRocMQu6Tzr1LUzBTQvXjarCxeb94kSTqvYx"),
);
const AGAVE_RENT_PER_BYTE = 6960;
const pda = (seeds, program) => PublicKey.findProgramAddressSync(seeds, program)[0];
const oracleData = pda([Buffer.from("oracle"), ORACLE.publicKey.toBuffer()], VRF);
const queue = (index) => pda([Buffer.from("queue"), ORACLE.publicKey.toBuffer(), Buffer.from([index])], VRF);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// vrfPublicKey returns the point of solana-vrf's generate_vrf_keypair. It
// takes HKDF-SHA512 of the keypair, reads the first 32 bytes as a scalar, and
// multiplies Ristretto's base point by it.
function vrfPublicKey(keypair) {
    const okm = Buffer.from(crypto.hkdfSync("sha512", keypair.secretKey, "VRF-Solana-SecretKey", "VRF-Key", 64));
    const L = 2n ** 252n + 27742317777372353535851937790883648493n;
    const scalar = BigInt("0x" + Buffer.from(okm.subarray(0, 32)).reverse().toString("hex")) % L;
    return Buffer.from(RistrettoPoint.BASE.multiply(scalar).toRawBytes());
}

// discriminated is a steel account: an 8-byte discriminator, then the bytes.
const discriminated = (d, bytes) => Buffer.concat([Buffer.from([d, 0, 0, 0, 0, 0, 0, 0]), bytes]);
const u64 = (n) => {
    const b = Buffer.alloc(8);
    b.writeBigUInt64LE(BigInt(n));
    return b;
};

function accounts(identity, dir) {
    const rows = {
        "validator-fees-vault": [pda([Buffer.from("v-fees-vault"), new PublicKey(identity).toBuffer()], DLP), DLP, Buffer.alloc(8)],
        "protocol-fees-vault": [pda([Buffer.from("fees-vault")], DLP), DLP, Buffer.alloc(8)],
        // Oracles { oracles: Vec<Pubkey> }, borsh, discriminator 0.
        "vrf-oracles": [pda([Buffer.from("oracles")], VRF), VRF, discriminated(0, Buffer.concat([Buffer.from([1, 0, 0, 0]), ORACLE.publicKey.toBuffer()]))],
        // Oracle { vrf_pubkey, registration_slot: 0, open_queue: 0 }, discriminator 1.
        "vrf-oracle": [oracleData, VRF, discriminated(1, Buffer.concat([vrfPublicKey(ORACLE), u64(0), u64(0)]))],
    };
    fs.mkdirSync(dir, { recursive: true });
    for (const [name, [address, owner, data]] of Object.entries(rows)) {
        const file = path.join(dir, `${name}.json`);
        const account = { lamports: (128 + data.length) * AGAVE_RENT_PER_BYTE, data: [data.toString("base64"), "base64"], owner: owner.toBase58(), executable: false, rentEpoch: 0, space: data.length };
        fs.writeFileSync(file, JSON.stringify({ pubkey: address.toBase58(), account }));
        console.log(`${address.toBase58()} ${file}`);
    }
}

function result(test, error, value) {
    fs.writeSync(3, JSON.stringify(error ? { test, ok: false, error } : { test, ok: true, ...(value === undefined ? {} : { value }) }) + "\n");
}

// queues waits for the 200 slots after the oracle's registration that
// InitializeOracleQueue requires. It then sends, signed by the oracle, what
// vrf-cli's initialize-oracle-queue and delegate-oracle-queue send. Queue 0 is
// the base chain's, and queue 1 is the rollup's.
async function queues(baseUrl) {
    const base = new Connection(baseUrl, "confirmed");
    while ((await base.getSlot()) < 201) await sleep(1000);
    const send = async (test, keys, data) => {
        try {
            await sendAndConfirmTransaction(base, new Transaction().add(new TransactionInstruction({ programId: VRF, keys, data })), [ORACLE]);
            result(test);
        } catch (e) {
            result(test, `${e.message}\n${(e.logs ?? []).join("\n")}`);
        }
    };
    const w = (pubkey, isSigner = false) => ({ pubkey, isSigner, isWritable: true });
    const r = (pubkey, isSigner = false) => ({ pubkey, isSigner, isWritable: false });
    for (const index of [0, 1]) {
        const data = Buffer.alloc(16);
        data[0] = 2;
        data.writeUInt32LE(9500, 8);
        data[12] = index;
        await send(`the oracle opens queue ${index}`, [w(ORACLE.publicKey, true), r(ORACLE.publicKey, true), w(oracleData), w(queue(index)), r(SystemProgram.programId)], data);
    }
    const q = queue(1);
    await send(
        "the oracle delegates queue 1 to the rollup",
        [
            w(ORACLE.publicKey, true),
            w(q),
            w(pda([Buffer.from("buffer"), q.toBuffer()], VRF)),
            w(pda([Buffer.from("delegation"), q.toBuffer()], DLP)),
            w(pda([Buffer.from("delegation-metadata"), q.toBuffer()], DLP)),
            r(DLP),
            r(VRF),
            r(SystemProgram.programId),
        ],
        Buffer.from([5, 0, 0, 0, 0, 0, 0, 0, 1]),
    );
}

// counter checks a state that the app's own test does not wait for. After the
// last step, the app owns the counter again on the base chain, and the counter
// holds the count that the rollup reached. counter waits for that state, and
// after a minute fails with what it saw. Anchor's 8-byte discriminator comes
// first in the account data.
async function counter(baseUrl, erUrl, app) {
    const address = pda([Buffer.from("counter")], new PublicKey(app));
    const [base, er] = [new Connection(baseUrl, "confirmed"), new Connection(erUrl, "confirmed")];
    const count = (a) => (a ? a.data.readBigUInt64LE(8) : null);
    const test = "the counter ends undelegated on the base chain with the rollup's count";
    for (const deadline = Date.now() + 60000; ; await sleep(500)) {
        const [onBase, onEr] = [await base.getAccountInfo(address), await er.getAccountInfo(address)];
        if (onBase && onBase.owner.equals(new PublicKey(app)) && count(onBase) === count(onEr)) {
            return result(test, undefined, String(count(onBase)));
        }
        if (Date.now() > deadline) {
            return result(test, `after 60 s the base chain's counter is owned by ${onBase ? onBase.owner.toBase58() : "nobody"} ` +
                `with count ${count(onBase)}, and the rollup's count is ${count(onEr)}`);
        }
    }
}

// unlisted checks admission. The delegation program refuses a key as a
// validator if genesis wrote no fees vault for it. Every validator instruction
// loads the caller's fees vault first, and fails with "Invalid account owner
// for validator fees vault". Two of them are a commit and CallHandler.
// CallHandler needs no delegated account. The payer tops up its escrow
// ["balance", payer, 0], and the key asks to move the escrow with a System
// transfer (tag 15, escrow index 0, then the action).
async function unlisted(baseUrl, payerFile) {
    const base = new Connection(baseUrl, "confirmed");
    const payer = Keypair.fromSecretKey(Uint8Array.from(JSON.parse(fs.readFileSync(payerFile))));
    const key = Keypair.generate();
    const sink = Keypair.generate().publicKey;
    const escrow = pda([Buffer.from("balance"), payer.publicKey.toBuffer(), Buffer.from([0])], DLP);
    const vault = pda([Buffer.from("v-fees-vault"), key.publicKey.toBuffer()], DLP);
    const u32 = (n) => {
        const b = Buffer.alloc(4);
        b.writeUInt32LE(n);
        return b;
    };
    const meta = (pubkey, isSigner, isWritable) => ({ pubkey, isSigner, isWritable });
    const send = (ix, signers) => sendAndConfirmTransaction(base, new Transaction().add(ix), signers, { commitment: "confirmed" });
    const test = "a key with no fees vault is refused as a validator";
    // TopUpEphemeralBalance { amount, index }, tag 9.
    await send(new TransactionInstruction({ programId: DLP, data: Buffer.concat([u64(9), u64(1_000_000_000), Buffer.from([0])]),
        keys: [meta(payer.publicKey, true, true), meta(payer.publicKey, false, false), meta(escrow, false, true), meta(SystemProgram.programId, false, false)] }), [payer]);
    await send(SystemProgram.transfer({ fromPubkey: payer.publicKey, toPubkey: key.publicKey, lamports: 1_000_000_000 }), [payer]);
    try {
        await send(new TransactionInstruction({ programId: DLP, data: Buffer.concat([u64(15), Buffer.from([0]), u32(12), u32(2), u64(1)]),
            keys: [meta(key.publicKey, true, true), meta(vault, false, true), meta(SystemProgram.programId, false, false),
                meta(payer.publicKey, false, false), meta(escrow, false, true), meta(escrow, false, true), meta(sink, false, true)] }), [key]);
    } catch (e) {
        const logs = (e.logs ?? []).join("\n");
        if (logs.includes("Invalid account owner for validator fees vault")) return result(test, undefined, "Invalid account owner for validator fees vault");
        return result(test, `refused another way: ${e.message}\n${logs}`);
    }
    result(test, "the key with no fees vault moved the escrow");
}

const secret = (file) => console.log(bs58.encode(Buffer.from(JSON.parse(fs.readFileSync(file)))));

// oracle prints the built-in oracle's IDENTITY:VRF_PUBKEY, the same value that
// `vrf-cli derive-pubkey` prints for its keypair.
const oracle = () => console.log(`${ORACLE.publicKey.toBase58()}:${bs58.encode(vrfPublicKey(ORACLE))}`);

const [mode, ...args] = process.argv.slice(2);
Promise.resolve({ accounts, queues, counter, unlisted, secret, oracle }[mode](...args)).then(
    () => process.exit(0),
    (e) => {
        console.error(e);
        process.exit(1);
    },
);
