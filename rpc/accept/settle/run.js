// The checks of the settlement run: proof verifiers and Squads v4 on one base
// chain. Each result is a line on fd 3.
//
//   node run.js sp1 RPC PAYER PROGRAM PROOF_FILE
//   node run.js r0 RPC OWNER VERIFIER ROUTER RECEIPT_RS
//   node run.js squads-create RPC PAYER STATE          a 2-of-3 multisig; STATE gets its vault
//   node run.js squads-upgrade RPC STATE PROGRAM BUFFER
//
// A value is what both chains must agree on: compute units where the inputs
// are the same, and the code that a refusal carries. The Squads multisig and
// members are new keys on each run, so their units are not compared.
const crypto = require("crypto");
const fs = require("fs");
const multisig = require("@sqds/multisig");
const {
    ComputeBudgetProgram,
    Connection,
    Keypair,
    LAMPORTS_PER_SOL,
    PublicKey,
    SystemProgram,
    SYSVAR_CLOCK_PUBKEY,
    SYSVAR_RENT_PUBKEY,
    Transaction,
    TransactionInstruction,
    TransactionMessage,
    sendAndConfirmTransaction,
} = require("@solana/web3.js");

const LOADER_V3 = new PublicKey("BPFLoaderUpgradeab1e11111111111111111111111");
const BN254_P = 21888242871839275222246405745257275088696311157297823662689037894645226208583n;
const load = (file) => Keypair.fromSecretKey(Uint8Array.from(JSON.parse(fs.readFileSync(file))));
const disc = (name) => crypto.createHash("sha256").update(`global:${name}`).digest().subarray(0, 8);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function result(test, error, value) {
    fs.writeSync(3, JSON.stringify(error ? { test, ok: false, error } : { test, ok: true, ...(value === undefined ? {} : { value: String(value) }) }) + "\n");
}

// check runs one case. f returns the case's value, or throws.
async function check(test, f) {
    try {
        result(test, null, await f());
    } catch (e) {
        result(test, `${e.message}\n${(e.logs || []).join("\n")}`);
    }
}

// refusal is the code a failed transaction carries: a program's custom code,
// or the runtime's description of the instruction error.
function refusal(e) {
    const text = `${e.message} ${(e.logs || []).join(" ")}`;
    const m = /custom program error: (0x[0-9a-f]+)/.exec(text) || /"Custom":(\d+)/.exec(text) || /Error processing Instruction \d+: ([^\n.]+)/.exec(text);
    if (!m) throw e;
    return m[1];
}

// send lands ixs with a full compute budget and returns the units used.
async function send(connection, payer, ...ixs) {
    const tx = new Transaction().add(ComputeBudgetProgram.setComputeUnitLimit({ units: 1_400_000 }), ...ixs);
    const sig = await sendAndConfirmTransaction(connection, tx, [payer], { commitment: "confirmed" });
    return (await connection.getTransaction(sig, { commitment: "confirmed", maxSupportedTransactionVersion: 0 })).meta.computeUnitsConsumed;
}

async function refused(connection, payer, ...ixs) {
    try {
        await send(connection, payer, ...ixs);
    } catch (e) {
        return refusal(e);
    }
    throw new Error("the tampered proof landed");
}

// afterDeploy waits two slots, because a program runs from the block after its deploy.
async function afterDeploy(connection) {
    const from = await connection.getSlot();
    while ((await connection.getSlot()) < from + 2) await sleep(300);
}

// sp1 sends sp1-solana's fixture proof (proofs/fibonacci_proof.bin, SP1 v5.0.0
// Groth16) to its example verifier, then the proof with one byte flipped. The
// fixture is bincode SP1ProofWithPublicValues: a u32 variant (3, Groth16), two
// u64-prefixed public input strings, the u64-prefixed hex encoded proof, the
// u64-prefixed hex raw proof, the 32-byte vkey hash, then the u64-prefixed
// public values. proof.bytes() is the vkey hash's first 4 bytes and the
// decoded encoded proof.
async function sp1(url, payerFile, programId, proofFile) {
    const connection = new Connection(url, "confirmed");
    const payer = load(payerFile);
    const b = fs.readFileSync(proofFile);
    let at = 4;
    const u64 = () => Number(b.readBigUInt64LE((at += 8) - 8));
    const bytes = (n) => b.subarray(at, (at += n));
    const string = () => bytes(u64()).toString();
    if (b.readUInt32LE(0) !== 3) throw new Error("not a Groth16 proof");
    string();
    string();
    const encoded = Buffer.from(string(), "hex");
    string();
    const vkeyHash = bytes(32);
    const publicValues = bytes(u64());
    const proof = Buffer.concat([vkeyHash.subarray(0, 4), encoded]);
    const vec = (x) => Buffer.concat([Buffer.from(Uint32Array.of(x.length).buffer), x]);
    const ix = (p) => new TransactionInstruction({
        programId: new PublicKey(programId),
        keys: [{ pubkey: payer.publicKey, isSigner: false, isWritable: true }],
        data: Buffer.concat([vec(p), vec(publicValues)]),
    });
    await afterDeploy(connection);
    await check("the fixture proof verifies", () => send(connection, payer, ix(proof)));
    const bad = Buffer.from(proof);
    bad[40] ^= 1;
    await check("a proof with one byte flipped is refused", () => refused(connection, payer, ix(bad)));
}

// fixture turns RISC Zero's FIB_RECEIPT (groth_16_verifier/src/v3_test_receipt.rs
// at risc0-solana v3.0.0) into verifier input as the crate's own test does: the
// seal's 256 bytes with pi_a negated, FIB_ID as little-endian words, and the
// SHA-256 of the 8-byte journal.
function fixture(file) {
    const src = fs.readFileSync(file, "utf8");
    const words = src.match(/FIB_ID: \[u32; 8\] = \[([^\]]+)\]/)[1].split(",").map((w) => Number(w.trim()));
    const receipt = Buffer.from(src.match(/FIB_RECEIPT: &\[u8\] = &\[([^\]]+)\]/)[1].split(",").map((x) => Number(x.trim())));
    // Receipt { inner: InnerReceipt::Groth16 (variant 2) { seal: Vec<u8>, .. }, journal, metadata }
    if (receipt.readUInt32LE(0) !== 2 || receipt.readBigUInt64LE(4) !== 256n) throw new Error("not a Groth16 receipt");
    const seal = receipt.subarray(12, 268);
    // The journal is the last Vec<u8> before the 32-byte verifier parameters digest.
    if (receipt.readBigUInt64LE(receipt.length - 48) !== 8n) throw new Error("the journal is not 8 bytes");
    const journal = receipt.subarray(receipt.length - 40, receipt.length - 32);
    const y = BigInt("0x" + seal.subarray(32, 64).toString("hex"));
    const negY = Buffer.from(((BN254_P - y) % BN254_P).toString(16).padStart(64, "0"), "hex");
    const imageId = Buffer.alloc(32);
    words.forEach((w, i) => imageId.writeUInt32LE(w >>> 0, i * 4));
    return {
        proof: Buffer.concat([seal.subarray(0, 32), negY, seal.subarray(64, 256)]),
        imageId,
        journalDigest: crypto.createHash("sha256").update(journal).digest(),
        selector: receipt.subarray(receipt.length - 32, receipt.length - 28),
    };
}

async function r0(url, ownerFile, verifierId, routerId, receiptRs) {
    const connection = new Connection(url, "confirmed");
    const owner = load(ownerFile);
    const [verifier, router] = [new PublicKey(verifierId), new PublicKey(routerId)];
    const { proof, imageId, journalDigest, selector } = fixture(receiptRs);
    const tampered = Buffer.from(proof);
    tampered[200] ^= 1;
    const meta = (pubkey, isWritable = false, isSigner = false) => ({ pubkey, isSigner, isWritable });
    const direct = (p) => new TransactionInstruction({
        programId: verifier,
        keys: [meta(SystemProgram.programId)],
        data: Buffer.concat([disc("verify"), p, imageId, journalDigest]),
    });
    const [routerPda] = PublicKey.findProgramAddressSync([Buffer.from("router")], router);
    const [entry] = PublicKey.findProgramAddressSync([Buffer.from("verifier"), selector], router);
    const [programData] = PublicKey.findProgramAddressSync([verifier.toBuffer()], LOADER_V3);
    const routed = (p) => new TransactionInstruction({
        programId: router,
        keys: [meta(routerPda), meta(entry), meta(verifier), meta(SystemProgram.programId)],
        data: Buffer.concat([disc("verify"), selector, p, imageId, journalDigest]),
    });
    await afterDeploy(connection);
    await check("the verifier accepts the fixture receipt", () => send(connection, owner, direct(proof)));
    await check("the verifier refuses a proof with one byte flipped", () => refused(connection, owner, direct(tampered)));
    await check("the router initializes and adds the verifier", async () => {
        await send(connection, owner, new TransactionInstruction({
            programId: router,
            keys: [meta(routerPda, true), meta(owner.publicKey, true, true), meta(SystemProgram.programId)],
            data: disc("initialize"),
        }));
        await send(connection, owner, new TransactionInstruction({
            programId: router,
            keys: [meta(routerPda), meta(entry, true), meta(programData), meta(verifier), meta(owner.publicKey, true, true), meta(SystemProgram.programId)],
            data: Buffer.concat([disc("add_verifier"), selector]),
        }));
    });
    await check("the router accepts the fixture receipt", () => send(connection, owner, routed(proof)));
    await check("the router refuses a proof with one byte flipped", () => refused(connection, owner, routed(tampered)));
}

async function confirmed(connection, signature) {
    const { value } = await connection.confirmTransaction(signature, "confirmed");
    if (value.err) throw new Error(`${signature} failed: ${JSON.stringify(value.err)}`);
}

// squads-create makes a 2-of-3 multisig through Squads' own SDK, as
// Hyperlane's operators do on Solana, and writes its vault and members.
async function squadsCreate(url, payerFile, stateFile) {
    const connection = new Connection(url, "confirmed");
    const payer = load(payerFile);
    const members = [Keypair.generate(), Keypair.generate(), Keypair.generate()];
    await check("a 2-of-3 multisig is created", async () => {
        const fund = members.map((m) => SystemProgram.transfer({ fromPubkey: payer.publicKey, toPubkey: m.publicKey, lamports: LAMPORTS_PER_SOL }));
        await sendAndConfirmTransaction(connection, new Transaction().add(...fund), [payer], { commitment: "confirmed" });
        const [configPda] = multisig.getProgramConfigPda({});
        const config = await multisig.accounts.ProgramConfig.fromAccountAddress(connection, configPda);
        const createKey = Keypair.generate();
        const [multisigPda] = multisig.getMultisigPda({ createKey: createKey.publicKey });
        await confirmed(connection, await multisig.rpc.multisigCreateV2({
            connection,
            treasury: config.treasury,
            createKey,
            creator: payer,
            multisigPda,
            configAuthority: null,
            threshold: 2,
            members: members.map((m) => ({ key: m.publicKey, permissions: multisig.types.Permissions.all() })),
            timeLock: 0,
            rentCollector: null,
        }));
        const [vault] = multisig.getVaultPda({ multisigPda, index: 0 });
        fs.writeFileSync(stateFile, JSON.stringify({ multisig: multisigPda.toBase58(), vault: vault.toBase58(), members: members.map((m) => Array.from(m.secretKey)) }));
        return config.multisigCreationFee.toString();
    });
}

// squads-upgrade proposes a loader v3 Upgrade of PROGRAM from BUFFER, signed
// by the vault. One approval must not execute it. Two approvals must.
async function squadsUpgrade(url, stateFile, programId, bufferId) {
    const connection = new Connection(url, "confirmed");
    const state = JSON.parse(fs.readFileSync(stateFile));
    const multisigPda = new PublicKey(state.multisig);
    const vault = new PublicKey(state.vault);
    const [m1, m2] = state.members.map((k) => Keypair.fromSecretKey(Uint8Array.from(k)));
    const program = new PublicKey(programId);
    const buffer = new PublicKey(bufferId);
    const [programData] = PublicKey.findProgramAddressSync([program.toBuffer()], LOADER_V3);
    const deployedAt = async () => (await connection.getAccountInfo(programData)).data.readBigUInt64LE(4);
    const transactionIndex = 1n;
    // execute carries the vault's instructions as CPIs, so it gets a full budget.
    const execute = async () => {
        const { instruction } = await multisig.instructions.vaultTransactionExecute({ connection, multisigPda, transactionIndex, member: m1.publicKey });
        return send(connection, m1, instruction);
    };
    const before = await deployedAt();
    // UpgradeableLoaderInstruction::Upgrade, variant 3.
    const upgradeIx = new TransactionInstruction({
        programId: LOADER_V3,
        keys: [
            { pubkey: programData, isSigner: false, isWritable: true },
            { pubkey: program, isSigner: false, isWritable: true },
            { pubkey: buffer, isSigner: false, isWritable: true },
            { pubkey: m1.publicKey, isSigner: false, isWritable: true },
            { pubkey: SYSVAR_RENT_PUBKEY, isSigner: false, isWritable: false },
            { pubkey: SYSVAR_CLOCK_PUBKEY, isSigner: false, isWritable: false },
            { pubkey: vault, isSigner: true, isWritable: false },
        ],
        data: Buffer.from([3, 0, 0, 0]),
    });
    await check("a member proposes the vault's upgrade and approves it", async () => {
        const message = new TransactionMessage({ payerKey: vault, recentBlockhash: (await connection.getLatestBlockhash()).blockhash, instructions: [upgradeIx] });
        await confirmed(connection, await multisig.rpc.vaultTransactionCreate({
            connection, feePayer: m1, multisigPda, transactionIndex, creator: m1.publicKey, vaultIndex: 0, ephemeralSigners: 0, transactionMessage: message,
        }));
        await confirmed(connection, await multisig.rpc.proposalCreate({ connection, feePayer: m1, creator: m1, multisigPda, transactionIndex }));
        await confirmed(connection, await multisig.rpc.proposalApprove({ connection, feePayer: m1, member: m1, multisigPda, transactionIndex }));
    });
    await check("one approval does not execute it", async () => {
        try {
            await execute();
        } catch (e) {
            return refusal(e);
        }
        throw new Error("executed on one approval");
    });
    await check("two approvals execute the upgrade", async () => {
        await confirmed(connection, await multisig.rpc.proposalApprove({ connection, feePayer: m2, member: m2, multisigPda, transactionIndex }));
        await execute();
        if ((await deployedAt()) === before) throw new Error("the program data's deploy slot did not move");
        if (await connection.getAccountInfo(buffer)) throw new Error("the buffer was not closed");
    });
}

const modes = { sp1, r0, "squads-create": squadsCreate, "squads-upgrade": squadsUpgrade };
const [mode, ...args] = process.argv.slice(2);
Promise.resolve(modes[mode](...args)).then(
    () => process.exit(0),
    (e) => {
        console.error(e);
        process.exit(1);
    },
);
