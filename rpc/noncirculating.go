package rpc

// nonCirculatingAccounts takes the place of Agave's non_circulating_accounts().
// Agave lists mainnet's 214 addresses on every cluster, but they are mainnet's
// own accounts and nothing on this chain is locked, so the list is empty.
// Agave's stake half is empty here too, because no program writes stake
// account data.
var nonCirculatingAccounts = []string{}
