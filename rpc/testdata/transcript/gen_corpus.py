#!/usr/bin/env python3
"""Prints the corpus: the requests that the Agave transcript records.

Each of the 53 methods and 9 subscriptions of Agave v4.3.0 has at least one
case, and so does each refusal that this chain makes on purpose. Placeholders
("$payer", "$slot.transfer") name what the shared setup creates. The setup is
in rpc/transcript_test.go, which runs this file for the corpus, so no output of
this file is committed. agave.json records the sha256 of the corpus. An edit
here fails the replay until the transcript is recorded again.

Order matters. Reads come first, then subscriptions, whose triggers add
blocks, then sends, then the HTTP layer.
"""
import json
import sys

cases = []


def rpc(name, method, *params, raw_params=None):
    c = {"name": name, "rpc": {"method": method}}
    if raw_params is not None:
        c["rpc"]["params"] = raw_params
    elif params:
        c["rpc"]["params"] = list(params)
    cases.append(c)


def body(name, text):
    cases.append({"name": name, "body": text})


def ws(name, method, *params, trigger=None, notifications=0):
    c = {"name": name, "ws": {"method": method}}
    if params:
        c["ws"]["params"] = list(params)
    if trigger:
        c["ws"]["trigger"] = trigger
        c["ws"]["notifications"] = notifications or 1
    cases.append(c)


def http(name, method, path, headers=None, text="", pad=0):
    h = {"method": method, "path": path}
    if headers:
        h["headers"] = headers
    if text:
        h["body"] = text
    if pad:
        h["pad"] = pad
    cases.append({"name": name, "http": h})


FAR = 999_999_999
B64 = {"encoding": "base64"}

# minimal set
rpc("getBalance/payer", "getBalance", "$payer")
rpc("getBalance/finalized", "getBalance", "$payer", {"commitment": "finalized"})
rpc("getBalance/processed", "getBalance", "$payer", {"commitment": "processed"})
rpc("getBalance/bad-commitment", "getBalance", "$payer", {"commitment": "nonsense"})
rpc("getBalance/min-context-slot-ahead", "getBalance", "$payer", {"minContextSlot": FAR})
rpc("getBalance/bad-key", "getBalance", "notakey")
rpc("getBalance/short-key", "getBalance", "1111")
rpc("getBalance/unknown", "getBalance", "$unfunded")
rpc("getEpochInfo/default", "getEpochInfo")
rpc("getEpochInfo/min-context-slot-ahead", "getEpochInfo", {"minContextSlot": FAR})
rpc("getGenesisHash", "getGenesisHash")
rpc("getHealth", "getHealth")
rpc("getIdentity", "getIdentity")
rpc("getSlot/default", "getSlot")
rpc("getSlot/processed", "getSlot", {"commitment": "processed"})
rpc("getSlot/min-context-slot-ahead", "getSlot", {"minContextSlot": FAR})
rpc("getBlockHeight/default", "getBlockHeight")
rpc("getBlockHeight/min-context-slot-ahead", "getBlockHeight", {"minContextSlot": FAR})
rpc("getHighestSnapshotSlot", "getHighestSnapshotSlot")
rpc("getTransactionCount/default", "getTransactionCount")
rpc("getVersion", "getVersion")
rpc("getVoteAccounts/default", "getVoteAccounts")
rpc("getVoteAccounts/bad-vote-pubkey", "getVoteAccounts", {"votePubkey": "notakey"})
rpc("getLeaderSchedule/not-a-leader", "getLeaderSchedule", None, {"identity": "$payer"})
rpc("getLeaderSchedule/far-slot", "getLeaderSchedule", FAR)
rpc("getLeaderSchedule/bad-identity", "getLeaderSchedule", None, {"identity": "notakey"})

# bank data
rpc("getMinimumBalanceForRentExemption/zero", "getMinimumBalanceForRentExemption", 0)
rpc("getMinimumBalanceForRentExemption/token-account", "getMinimumBalanceForRentExemption", 165)
rpc("getMinimumBalanceForRentExemption/over-10-MiB", "getMinimumBalanceForRentExemption", 10 * 1024 * 1024 + 1)
rpc("getInflationGovernor", "getInflationGovernor")
rpc("getInflationRate", "getInflationRate")
rpc("getEpochSchedule", "getEpochSchedule")
rpc("getSlotLeader/default", "getSlotLeader")
rpc("getSlotLeaders/from-transfer", "getSlotLeaders", "$slot.transfer", 5)
rpc("getSlotLeaders/limit-over-5000", "getSlotLeaders", "$slot.transfer", 5001)
rpc("getSlotLeaders/far", "getSlotLeaders", FAR, 1)
rpc("getAgGenesisCert", "getAgGenesisCert")
rpc("getBlockProduction/default", "getBlockProduction")
rpc("getBlockProduction/bad-range", "getBlockProduction", {"range": {"firstSlot": 10, "lastSlot": 5}})

# accounts data
rpc("getAccountInfo/payer-default", "getAccountInfo", "$payer")
rpc("getAccountInfo/payer-base64", "getAccountInfo", "$payer", B64)
rpc("getAccountInfo/mint-base64", "getAccountInfo", "$mint", B64)
rpc("getAccountInfo/mint-base58", "getAccountInfo", "$mint", {"encoding": "base58"})
rpc("getAccountInfo/mint-binary", "getAccountInfo", "$mint", {"encoding": "binary"})
rpc("getAccountInfo/token-account-base58-over-128", "getAccountInfo", "$tokenA", {"encoding": "base58"})
rpc("getAccountInfo/mint-jsonParsed", "getAccountInfo", "$mint", {"encoding": "jsonParsed"})
rpc("getAccountInfo/token-account-jsonParsed", "getAccountInfo", "$tokenA", {"encoding": "jsonParsed"})
rpc("getAccountInfo/mint-zstd", "getAccountInfo", "$mint", {"encoding": "base64+zstd"})
rpc("getAccountInfo/data-slice", "getAccountInfo", "$mint", {"encoding": "base64", "dataSlice": {"offset": 4, "length": 8}})
rpc("getAccountInfo/unknown", "getAccountInfo", "$unfunded", B64)
rpc("getAccountInfo/bad-key", "getAccountInfo", "notakey")
rpc("getAccountInfo/min-context-slot-ahead", "getAccountInfo", "$payer", {"encoding": "base64", "minContextSlot": FAR})
rpc("getMultipleAccounts/three", "getMultipleAccounts", ["$payer", "$mint", "$unfunded"], B64)
rpc("getMultipleAccounts/default-encoding", "getMultipleAccounts", ["$mint"])
rpc("getMultipleAccounts/over-100", "getMultipleAccounts", ["$payer"] * 101)
rpc("getMultipleAccounts/bad-key", "getMultipleAccounts", ["$payer", "notakey"])
rpc("getBlockCommitment/transfer", "getBlockCommitment", "$slot.transfer")
rpc("getTokenAccountBalance/tokenA", "getTokenAccountBalance", "$tokenA")
rpc("getTokenAccountBalance/not-token", "getTokenAccountBalance", "$payer")
rpc("getTokenAccountBalance/unknown", "getTokenAccountBalance", "$unfunded")
rpc("getTokenSupply/mint", "getTokenSupply", "$mint")
rpc("getTokenSupply/not-mint", "getTokenSupply", "$tokenA")
rpc("getTokenSupply/unknown", "getTokenSupply", "$unfunded")

# accounts scan
rpc("getProgramAccounts/token-by-mint", "getProgramAccounts", "$tokenProgram",
    {"encoding": "base64", "filters": [{"dataSize": 165}, {"memcmp": {"offset": 0, "bytes": "$mint"}}]})
rpc("getProgramAccounts/token-with-context", "getProgramAccounts", "$tokenProgram",
    {"encoding": "base64", "withContext": True, "filters": [{"memcmp": {"offset": 0, "bytes": "$mint"}}]})
rpc("getProgramAccounts/token-jsonParsed", "getProgramAccounts", "$tokenProgram",
    {"encoding": "jsonParsed", "filters": [{"memcmp": {"offset": 0, "bytes": "$mint"}}]})
rpc("getProgramAccounts/token-account-state", "getProgramAccounts", "$tokenProgram",
    {"encoding": "base64", "filters": [{"tokenAccountState": None}, {"memcmp": {"offset": 0, "bytes": "$mint"}}]})
rpc("getProgramAccounts/data-slice", "getProgramAccounts", "$tokenProgram",
    {"encoding": "base64", "dataSlice": {"offset": 0, "length": 32}, "filters": [{"memcmp": {"offset": 0, "bytes": "$mint"}}]})
rpc("getProgramAccounts/five-filters", "getProgramAccounts", "$tokenProgram",
    {"filters": [{"dataSize": 165}] * 5})
rpc("getProgramAccounts/memcmp-over-128", "getProgramAccounts", "$tokenProgram",
    {"filters": [{"memcmp": {"offset": 0, "bytes": "1" * 200}}]})
rpc("getProgramAccounts/bad-key", "getProgramAccounts", "notakey")
rpc("getLargestAccounts/default", "getLargestAccounts")
rpc("getLargestAccounts/circulating", "getLargestAccounts", {"filter": "circulating"})
rpc("getLargestAccounts/non-circulating", "getLargestAccounts", {"filter": "nonCirculating"})
rpc("getSupply/default", "getSupply")
rpc("getSupply/exclude-list", "getSupply", {"excludeNonCirculatingAccountsList": True})
rpc("getTokenLargestAccounts/mint", "getTokenLargestAccounts", "$mint")
rpc("getTokenLargestAccounts/not-mint", "getTokenLargestAccounts", "$tokenA")
rpc("getTokenAccountsByOwner/payer-mint", "getTokenAccountsByOwner", "$payer", {"mint": "$mint"}, B64)
rpc("getTokenAccountsByOwner/payer-program", "getTokenAccountsByOwner", "$payer", {"programId": "$tokenProgram"}, B64)
rpc("getTokenAccountsByOwner/payer-jsonParsed", "getTokenAccountsByOwner", "$payer", {"programId": "$tokenProgram"}, {"encoding": "jsonParsed"})
rpc("getTokenAccountsByOwner/default-encoding", "getTokenAccountsByOwner", "$payer", {"mint": "$mint"})
rpc("getTokenAccountsByOwner/not-token-program", "getTokenAccountsByOwner", "$payer", {"programId": "$systemProgram"})
rpc("getTokenAccountsByOwner/unknown-mint", "getTokenAccountsByOwner", "$payer", {"mint": "$unfunded"})
rpc("getTokenAccountsByOwner/not-mint", "getTokenAccountsByOwner", "$payer", {"mint": "$tokenA"})
rpc("getTokenAccountsByOwner/no-filter", "getTokenAccountsByOwner", "$payer", {})
rpc("getTokenAccountsByDelegate/mint", "getTokenAccountsByDelegate", "$delegate", {"mint": "$mint"}, B64)
rpc("getTokenAccountsByDelegate/program", "getTokenAccountsByDelegate", "$delegate", {"programId": "$tokenProgram"}, B64)

# full
rpc("getInflationReward/payer", "getInflationReward", ["$payer"])
rpc("getInflationReward/over-32", "getInflationReward", ["$payer"] * 33)
rpc("getClusterNodes", "getClusterNodes")
rpc("getRecentPerformanceSamples/default", "getRecentPerformanceSamples")
rpc("getRecentPerformanceSamples/over-720", "getRecentPerformanceSamples", 721)
rpc("getSignatureStatuses/transfer", "getSignatureStatuses", ["$sig.transfer"])
rpc("getSignatureStatuses/search-history", "getSignatureStatuses", ["$sig.transfer", "$sig.failed"], {"searchTransactionHistory": True})
rpc("getSignatureStatuses/empty-config", "getSignatureStatuses", ["$sig.transfer"], {})
rpc("getSignatureStatuses/over-256", "getSignatureStatuses", ["$sig.transfer"] * 257)
rpc("getSignatureStatuses/bad-signature", "getSignatureStatuses", ["notasignature"])
rpc("getMaxRetransmitSlot", "getMaxRetransmitSlot")
rpc("getMaxShredInsertSlot", "getMaxShredInsertSlot")
rpc("minimumLedgerSlot", "minimumLedgerSlot")
rpc("getBlock/transfer-default", "getBlock", "$slot.transfer")
rpc("getBlock/transfer-json", "getBlock", "$slot.transfer", {"encoding": "json", "maxSupportedTransactionVersion": 0})
rpc("getBlock/transfer-jsonParsed", "getBlock", "$slot.transfer", {"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0})
rpc("getBlock/transfer-base64", "getBlock", "$slot.transfer", {"encoding": "base64", "maxSupportedTransactionVersion": 0})
rpc("getBlock/transfer-signatures", "getBlock", "$slot.transfer", {"transactionDetails": "signatures"})
rpc("getBlock/transfer-none", "getBlock", "$slot.transfer", {"transactionDetails": "none", "rewards": False})
rpc("getBlock/transfer-accounts", "getBlock", "$slot.transfer", {"transactionDetails": "accounts", "maxSupportedTransactionVersion": 0})
rpc("getBlock/bare-string", "getBlock", "$slot.transfer", "base64")
rpc("getBlock/processed", "getBlock", "$slot.transfer", {"commitment": "processed"})
rpc("getBlock/above-committed", "getBlock", FAR)
rpc("getBlockTime/transfer", "getBlockTime", "$slot.transfer")
rpc("getBlockTime/genesis", "getBlockTime", 0)
rpc("getBlockTime/above-committed", "getBlockTime", FAR)
rpc("getBlocks/setup", "getBlocks", "$slot.transfer", "$slot.failed")
rpc("getBlocks/over-500000", "getBlocks", 0, 500_001)
rpc("getBlocks/end-before-start", "getBlocks", "$slot.failed", "$slot.transfer")
rpc("getBlocks/processed", "getBlocks", "$slot.transfer", "$slot.failed", {"commitment": "processed"})
rpc("getBlocksWithLimit/setup", "getBlocksWithLimit", "$slot.transfer", 3)
rpc("getBlocksWithLimit/over-500000", "getBlocksWithLimit", 0, 500_001)
rpc("getTransaction/transfer-default", "getTransaction", "$sig.transfer")
rpc("getTransaction/transfer-json", "getTransaction", "$sig.transfer", {"encoding": "json"})
rpc("getTransaction/transfer-json-v0", "getTransaction", "$sig.transfer", {"encoding": "json", "maxSupportedTransactionVersion": 0})
rpc("getTransaction/transfer-jsonParsed", "getTransaction", "$sig.transfer", {"encoding": "jsonParsed"})
rpc("getTransaction/transfer-base64", "getTransaction", "$sig.transfer", B64)
rpc("getTransaction/transfer-base58", "getTransaction", "$sig.transfer", {"encoding": "base58"})
rpc("getTransaction/transfer-binary", "getTransaction", "$sig.transfer", "binary")
rpc("getTransaction/token-transfer-base64", "getTransaction", "$sig.tokenTransfer", B64)
rpc("getTransaction/token-transfer-jsonParsed", "getTransaction", "$sig.tokenTransfer", {"encoding": "jsonParsed"})
rpc("getTransaction/memo-base64", "getTransaction", "$sig.memo", B64)
rpc("getTransaction/failed-base64", "getTransaction", "$sig.failed", B64)
rpc("getTransaction/processed", "getTransaction", "$sig.transfer", {"encoding": "base64", "commitment": "processed"})
rpc("getTransaction/unknown", "getTransaction", "1" * 64, B64)
rpc("getTransaction/bad-signature", "getTransaction", "notasignature", B64)
rpc("getSignaturesForAddress/payer", "getSignaturesForAddress", "$payer")
rpc("getSignaturesForAddress/limit-1", "getSignaturesForAddress", "$payer", {"limit": 1})
rpc("getSignaturesForAddress/before-transfer", "getSignaturesForAddress", "$payer", {"before": "$sig.mint"})
rpc("getSignaturesForAddress/unknown-before", "getSignaturesForAddress", "$payer", {"before": "1" * 64})
rpc("getSignaturesForAddress/limit-over-1000", "getSignaturesForAddress", "$payer", {"limit": 1001})
rpc("getSignaturesForAddress/limit-0", "getSignaturesForAddress", "$payer", {"limit": 0})
rpc("getSignaturesForAddress/processed", "getSignaturesForAddress", "$payer", {"commitment": "processed"})
rpc("getFirstAvailableBlock", "getFirstAvailableBlock")
rpc("getLatestBlockhash/default", "getLatestBlockhash")
rpc("getLatestBlockhash/processed", "getLatestBlockhash", {"commitment": "processed"})
rpc("getLatestBlockhash/min-context-slot-ahead", "getLatestBlockhash", {"minContextSlot": FAR})
rpc("isBlockhashValid/latest", "isBlockhashValid", "$blockhash")
rpc("isBlockhashValid/unknown", "isBlockhashValid", "1" * 32)
rpc("isBlockhashValid/bad", "isBlockhashValid", "notahash")
rpc("getFeeForMessage/transfer", "getFeeForMessage", "$message.transfer")
rpc("getFeeForMessage/garbage", "getFeeForMessage", "AAAA")
rpc("getStakeMinimumDelegation", "getStakeMinimumDelegation")
rpc("getRecentPrioritizationFees/default", "getRecentPrioritizationFees")
rpc("getRecentPrioritizationFees/payer", "getRecentPrioritizationFees", ["$payer"])
rpc("getRecentPrioritizationFees/over-128", "getRecentPrioritizationFees", ["$payer"] * 129)
rpc("simulateTransaction/valid", "simulateTransaction", "$tx.simulate", B64)
rpc("simulateTransaction/sig-verify", "simulateTransaction", "$tx.badSignature", {"encoding": "base64", "sigVerify": True})
rpc("simulateTransaction/replace-blockhash", "simulateTransaction", "$tx.simulate", {"encoding": "base64", "replaceRecentBlockhash": True})
rpc("simulateTransaction/sig-verify-and-replace", "simulateTransaction", "$tx.simulate", {"encoding": "base64", "sigVerify": True, "replaceRecentBlockhash": True})
rpc("simulateTransaction/accounts", "simulateTransaction", "$tx.simulate", {"encoding": "base64", "accounts": {"encoding": "base64", "addresses": ["$payer"]}})
rpc("simulateTransaction/inner-instructions", "simulateTransaction", "$tx.simulate", {"encoding": "base64", "innerInstructions": True})
rpc("simulateTransaction/unfunded", "simulateTransaction", "$tx.unfunded", B64)
rpc("simulateTransaction/json-encoding", "simulateTransaction", "$tx.simulate", {"encoding": "json"})

# framing
rpc("framing/unknown-method", "getNothing")
rpc("framing/params-object", "getBalance", raw_params={"pubkey": "$payer"})
rpc("framing/too-few-params", "getBalance")
rpc("framing/too-many-params", "getBalance", "$payer", {}, 3)
rpc("framing/params-not-expected", "getHealth", 1)
body("framing/not-json", "{not json")
body("framing/empty-batch", "[]")
body("framing/batch", '[{"jsonrpc":"2.0","id":1,"method":"getSlot"},{"jsonrpc":"2.0","id":2,"method":"getNothing"}]')
body("framing/notification", '{"jsonrpc":"2.0","method":"getSlot"}')
body("framing/no-version", '{"id":1,"method":"getSlot"}')

# subscriptions
ws("accountSubscribe/payer", "accountSubscribe", "$payer", {"encoding": "base64"}, trigger="transfer")
ws("accountSubscribe/payer-default", "accountSubscribe", "$payer", trigger="transfer")
ws("accountSubscribe/token-jsonParsed", "accountSubscribe", "$tokenB", {"encoding": "jsonParsed"}, trigger="tokenTransfer")
ws("accountSubscribe/bad-key", "accountSubscribe", "notakey")
ws("programSubscribe/token", "programSubscribe", "$tokenProgram", {"encoding": "base64", "filters": [{"memcmp": {"offset": 0, "bytes": "$mint"}}, {"memcmp": {"offset": 32, "bytes": "$recipient"}}]}, trigger="tokenTransfer")
ws("programSubscribe/five-filters", "programSubscribe", "$tokenProgram", {"filters": [{"dataSize": 165}] * 5})
ws("logsSubscribe/mentions-payer", "logsSubscribe", {"mentions": ["$payer"]}, trigger="transfer")
ws("logsSubscribe/all", "logsSubscribe", "all", trigger="transfer")
ws("logsSubscribe/two-mentions", "logsSubscribe", {"mentions": ["$payer", "$recipient"]})
ws("signatureSubscribe/trigger", "signatureSubscribe", "$sig.trigger", trigger="transfer")
ws("signatureSubscribe/received", "signatureSubscribe", "$sig.trigger", {"enableReceivedNotification": True}, trigger="transfer", notifications=2)
ws("signatureSubscribe/bad", "signatureSubscribe", "notasignature")
ws("slotSubscribe", "slotSubscribe", trigger="transfer")
ws("slotsUpdatesSubscribe", "slotsUpdatesSubscribe", trigger="transfer")
ws("rootSubscribe", "rootSubscribe", trigger="transfer")
ws("blockSubscribe/all", "blockSubscribe", "all")
ws("voteSubscribe", "voteSubscribe")
ws("ws/getVersion", "getVersion")
ws("ws/unsubscribe-unknown", "accountUnsubscribe", 999)

# sends, last, because they change state
rpc("sendTransaction/valid", "sendTransaction", "$tx.send", B64)
rpc("sendTransaction/base58-default", "sendTransaction", "$tx.send.base58")
rpc("sendTransaction/bad-signature", "sendTransaction", "$tx.badSignature", B64)
rpc("sendTransaction/bad-signature-skip-preflight", "sendTransaction", "$tx.badSignature", {"encoding": "base64", "skipPreflight": True})
rpc("sendTransaction/unfunded", "sendTransaction", "$tx.unfunded", B64)
rpc("sendTransaction/json-encoding", "sendTransaction", "$tx.send", {"encoding": "json"})
rpc("sendTransaction/not-a-transaction", "sendTransaction", "AAAA", B64)
rpc("sendTransaction/bad-base64", "sendTransaction", "%%%", B64)
rpc("requestAirdrop/payer", "requestAirdrop", "$payer", 1_000_000)

# the HTTP layer
http("http/get-root", "GET", "/")
http("http/options", "OPTIONS", "/", {"Origin": "http://example.com", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type"})
http("http/post-with-origin", "POST", "/", {"Origin": "http://example.com", "Content-Type": "application/json"}, '{"jsonrpc":"2.0","id":1,"method":"getSlot"}')
http("http/post-origin-null", "POST", "/", {"Origin": "null", "Content-Type": "application/json"}, '{"jsonrpc":"2.0","id":1,"method":"getSlot"}')
http("http/post-text-plain", "POST", "/", {"Content-Type": "text/plain"}, '{"jsonrpc":"2.0","id":1,"method":"getSlot"}')
http("http/post-over-50KiB", "POST", "/", {"Content-Type": "application/json"}, '{"jsonrpc":"2.0","id":1,"method":"getSlot"}', pad=51200)
http("http/health", "GET", "/health")
http("http/circulating-supply", "GET", "/v0/circulating-supply")
http("http/total-supply", "GET", "/v0/total-supply")
http("http/genesis", "GET", "/genesis.tar.bz2")
http("http/snapshot", "GET", "/snapshot.tar.bz2")

json.dump({"cases": cases}, sys.stdout, indent=1)
sys.stdout.write("\n")
