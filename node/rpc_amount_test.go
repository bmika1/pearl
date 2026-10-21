// Copyright (c) 2025-2026 The Pearl Research Labs
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/pearl-research-labs/pearl/node/btcjson"
	"github.com/pearl-research-labs/pearl/node/btcutil"
	"github.com/stretchr/testify/require"
)

func TestRPCAmountMarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		amount int64
		want   string
	}{
		{"zero", 0, "0.00000000"},
		{"one_grain", 1, "0.00000001"},
		{"fraction", 12345678, "0.12345678"},
		{"whole_pearl", btcutil.GrainPerPearl, "1.00000000"},
		{"negative_fraction", -1, "-0.00000001"},
		{"negative_whole_and_fraction", -100000001, "-1.00000001"},
		{"reported_minus_one", 6710886400000001, "67108864.00000001"},
		{"reported", 6710886400000002, "67108864.00000002"},
		{"reported_plus_one", 6710886400000003, "67108864.00000003"},
		{"float_integer_limit", 1 << 53, "90071992.54740992"},
		{"above_float_integer_limit", 1<<53 + 1, "90071992.54740993"},
		{"consensus_max_minus_one", btcutil.MaxGrain - 1, "20999999999.99999999"},
		{"consensus_max", btcutil.MaxGrain, "21000000000.00000000"},
		// decoderawtransaction can return amounts that consensus would reject.
		{"consensus_max_plus_one", btcutil.MaxGrain + 1, "21000000000.00000001"},
		{"int64_min", math.MinInt64, "-92233720368.54775808"},
		{"int64_max", math.MaxInt64, "92233720368.54775807"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(rpcAmount(test.amount))
			require.NoError(t, err)
			require.Equal(t, test.want, string(encoded))
			require.True(t, json.Valid(encoded))
		})
	}
}

// Compare decoded JSON so fixed decimal formatting and field order may differ,
// while field names, omission rules, nulls, arrays, and values must stay intact.
func requireRPCAmountJSONShape(t *testing.T, legacy, exact interface{}) []byte {
	t.Helper()
	legacyJSON, err := json.Marshal(legacy)
	require.NoError(t, err)
	exactJSON, err := json.Marshal(exact)
	require.NoError(t, err)
	require.JSONEq(t, string(legacyJSON), string(exactJSON))
	return exactJSON
}

func TestRPCAmountResultJSONCompatibility(t *testing.T) {
	t.Parallel()

	script := btcjson.ScriptPubKeyResult{
		Asm: "1 abc", Hex: "5120abcd", ReqSigs: 1, Type: "witness_v1_taproot",
		Address: "prl1example", Addresses: []string{"prl1legacy"},
	}
	legacyVout := btcjson.Vout{Value: 1.25, N: 2, ScriptPubKey: script}
	exactVout := rpcVout{Vout: legacyVout, Value: 125000000}
	// The server-owned value must shadow even a populated legacy float field.
	exactVout.Vout.Value = -999
	legacyPrevOut := btcjson.PrevOut{Addresses: []string{"prl1previous"}, Value: 1.25}
	exactPrevOut := rpcPrevOut{PrevOut: legacyPrevOut, Value: 125000000}
	exactPrevOut.PrevOut.Value = -999
	legacyTxOut := btcjson.GetTxOutResult{
		BestBlock: "best-block", Confirmations: 7, Value: 1.25,
		ScriptPubKey: script, Coinbase: true,
	}
	exactTxOut := rpcGetTxOutResult{GetTxOutResult: legacyTxOut, Value: 125000000}
	exactTxOut.GetTxOutResult.Value = -999

	legacyRaw := btcjson.TxRawResult{
		Hex: "transaction-hex", Txid: "txid", Hash: "wtxid", Vsize: 120,
		Size: 150, Version: 2, LockTime: 5,
		Vin:  []btcjson.Vin{{Coinbase: "03010203", Sequence: 4}},
		Vout: []btcjson.Vout{legacyVout}, BlockHash: "block-hash",
		Confirmations: 7, Time: 10, Blocktime: 11,
	}
	exactRaw := rpcTxRawResult{TxRawResult: legacyRaw, Vout: []rpcVout{exactVout}}
	exactRaw.TxRawResult.Vout = nil
	legacyDecoded := btcjson.TxRawDecodeResult{
		Txid: "txid", Version: 2, Locktime: 5,
		Vin: legacyRaw.Vin, Vout: legacyRaw.Vout,
	}
	exactDecoded := rpcTxRawDecodeResult{
		TxRawDecodeResult: legacyDecoded, Vout: []rpcVout{exactVout},
	}
	exactDecoded.TxRawDecodeResult.Vout = nil
	legacyBlock := btcjson.GetBlockVerboseResult{
		Hash: "block-hash", Confirmations: 7, Size: 150, StrippedSize: 120,
		Vsize: 130, Height: 8, Version: 2, VersionHex: "00000002",
		MerkleRoot: "merkle-root", RawTx: []btcjson.TxRawResult{legacyRaw},
		Time: 10, Bits: "1d00ffff", Difficulty: 1.5,
		PreviousHash: "previous-block", NextHash: "next-block", ProofCommitment: "proof",
	}
	exactBlock := rpcGetBlockVerboseResult{
		GetBlockVerboseResult: legacyBlock, RawTx: []rpcTxRawResult{exactRaw},
	}
	exactBlock.GetBlockVerboseResult.RawTx = nil
	legacySearch := btcjson.SearchRawTransactionsResult{
		Hex: "transaction-hex", Txid: "txid", Hash: "wtxid", Size: "150",
		Vsize: "120", Version: 2, LockTime: 5,
		Vin:  []btcjson.VinPrevOut{{Coinbase: "03010203", Sequence: 4}},
		Vout: legacyRaw.Vout, BlockHash: "block-hash", Confirmations: 7, Time: 10, Blocktime: 11,
	}
	exactSearch := rpcSearchRawTransactionsResult{
		SearchRawTransactionsResult: legacySearch,
		Vin:                         []rpcVinPrevOut{{Coinbase: "03010203", Sequence: 4}},
		Vout:                        []rpcVout{exactVout},
	}
	exactSearch.SearchRawTransactionsResult.Vin = nil
	exactSearch.SearchRawTransactionsResult.Vout = nil

	for _, test := range []struct {
		name   string
		legacy interface{}
		exact  interface{}
	}{
		{"output", legacyVout, exactVout},
		{"previous_output", legacyPrevOut, exactPrevOut},
		{"gettxout", legacyTxOut, exactTxOut},
		{"raw_transaction", legacyRaw, exactRaw},
		{"decoded_transaction", legacyDecoded, exactDecoded},
		{"verbose_block", legacyBlock, exactBlock},
		{"search_transaction", legacySearch, exactSearch},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded := requireRPCAmountJSONShape(t, test.legacy, test.exact)
			require.Equal(t, 1, bytes.Count(encoded, []byte(`"value":`)),
				"the embedded float field must not produce a duplicate value key")
			require.Contains(t, string(encoded), `"value":1.25000000`)
		})
	}

	t.Run("block_transaction_ids", func(t *testing.T) {
		legacy := btcjson.GetBlockVerboseResult{Tx: []string{"txid"}}
		exact := rpcGetBlockVerboseResult{GetBlockVerboseResult: legacy}
		for _, transactions := range [][]rpcTxRawResult{nil, {}} {
			exact.RawTx = transactions
			encoded := requireRPCAmountJSONShape(t, legacy, exact)
			require.NotContains(t, string(encoded), `"rawtx"`)
		}
	})
}

func TestRPCVinPrevOutJSONCompatibility(t *testing.T) {
	t.Parallel()

	previous := &btcjson.PrevOut{Addresses: []string{"prl1previous"}, Value: 1.25}
	tests := []struct {
		name string
		vin  btcjson.VinPrevOut
	}{
		{
			name: "coinbase_omits_other_fields",
			vin: btcjson.VinPrevOut{
				Coinbase: "03010203", Sequence: 9, Txid: "ignored", Vout: 2,
				Witness: []string{"ignored"}, PrevOut: previous,
			},
		},
		{
			name: "witness_with_previous_output",
			vin: btcjson.VinPrevOut{
				Txid: "txid", Vout: 2, ScriptSig: &btcjson.ScriptSig{Asm: "asm", Hex: "abcd"},
				Witness: []string{"aabb", "ccdd"}, PrevOut: previous, Sequence: 9,
			},
		},
		{
			name: "nonwitness_with_previous_output",
			vin: btcjson.VinPrevOut{
				Txid: "txid", Vout: 2, ScriptSig: &btcjson.ScriptSig{Asm: "asm", Hex: "abcd"},
				PrevOut: previous, Sequence: 9,
			},
		},
		{
			name: "witness_without_previous_output",
			vin:  btcjson.VinPrevOut{Txid: "txid", Vout: 2, Witness: []string{"aabb"}, Sequence: 9},
		},
		{
			name: "nonwitness_without_previous_output",
			vin:  btcjson.VinPrevOut{Txid: "txid", Vout: 2, Witness: []string{}, Sequence: 9},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exact := rpcVinPrevOut{
				Coinbase: test.vin.Coinbase, Txid: test.vin.Txid, Vout: test.vin.Vout,
				ScriptSig: test.vin.ScriptSig, Witness: test.vin.Witness, Sequence: test.vin.Sequence,
			}
			if test.vin.PrevOut != nil {
				exact.PrevOut = &rpcPrevOut{PrevOut: *test.vin.PrevOut, Value: 125000000}
				exact.PrevOut.PrevOut.Value = -999
			}
			// The legacy custom marshaler has a pointer receiver. Verify the new
			// result works both alone and when nested in a response slice.
			encoded := requireRPCAmountJSONShape(t, &test.vin, exact)
			requireRPCAmountJSONShape(t, []*btcjson.VinPrevOut{&test.vin}, []rpcVinPrevOut{exact})
			if test.vin.Coinbase == "" && test.vin.PrevOut != nil {
				require.Contains(t, string(encoded), `"value":1.25000000`)
			}
		})
	}
}
