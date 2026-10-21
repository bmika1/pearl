package rpcclient

import (
	"fmt"
	"testing"

	"github.com/pearl-research-labs/pearl/node/btcjson"
	"github.com/pearl-research-labs/pearl/node/btcutil"
	"github.com/pearl-research-labs/pearl/node/chaincfg/chainhash"
)

// TestTxAcceptedNotificationAmounts exercises the actual message dispatch path
// for both notification forms, including amounts that float64 cannot preserve.
func TestTxAcceptedNotificationAmounts(t *testing.T) {
	t.Parallel()

	const txID = "1111111111111111111111111111111111111111111111111111111111111111"
	tests := []struct {
		name   string
		value  string
		grains btcutil.Amount
	}{
		{name: "zero", value: "0.00000000", grains: 0},
		{name: "one grain", value: "0.00000001", grains: 1},
		{name: "exponent notation", value: "1e-8", grains: 1},
		{name: "reported amount", value: "67108864.00000002", grains: 6710886400000002},
		{name: "above exact float integers", value: "90071992.54740993", grains: 9007199254740993},
		{name: "maximum consensus amount", value: "21000000000.00000000", grains: btcutil.MaxGrain},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, verbose := range []bool{false, true} {
				t.Run(fmt.Sprintf("verbose=%t", verbose), func(t *testing.T) {
					var calls int
					var gotHash string
					var gotAmount btcutil.Amount
					client := &Client{ntfnHandlers: &NotificationHandlers{
						OnTxAccepted: func(hash *chainhash.Hash, amount btcutil.Amount) {
							if verbose {
								t.Fatal("called nonverbose handler for verbose notification")
							}
							calls++
							gotHash, gotAmount = hash.String(), amount
						},
						OnTxAcceptedVerbose: func(tx *btcjson.TxRawResult) {
							if !verbose {
								t.Fatal("called verbose handler for nonverbose notification")
							}
							if len(tx.Vout) != 1 {
								t.Fatalf("got %d outputs, want 1", len(tx.Vout))
							}
							calls++
							gotHash, gotAmount = tx.Txid, btcutil.Amount(tx.Vout[0].Value)
						},
					}}
					var message string
					if verbose {
						message = fmt.Sprintf(`{"jsonrpc":"1.0","id":null,"method":"txacceptedverbose","params":[{"txid":%q,"vout":[{"value":%s}]}]}`, txID, test.value)
					} else {
						message = fmt.Sprintf(`{"jsonrpc":"1.0","id":null,"method":"txaccepted","params":[%q,%s]}`, txID, test.value)
					}
					client.handleMessage([]byte(message))
					if calls != 1 || gotHash != txID || gotAmount != test.grains {
						t.Fatalf("got calls=%d hash=%s grains=%d; want calls=1 hash=%s grains=%d",
							calls, gotHash, int64(gotAmount), txID, int64(test.grains))
					}
				})
			}
		})
	}
}

// Malformed amounts must not reach either transaction notification callback.
func TestTxAcceptedNotificationInvalidAmounts(t *testing.T) {
	t.Parallel()

	const txID = "1111111111111111111111111111111111111111111111111111111111111111"
	for _, value := range []string{`"1.0"`, "null", "true", "0.000000001", "92233720368.54775808"} {
		t.Run(value, func(t *testing.T) {
			client := &Client{ntfnHandlers: &NotificationHandlers{
				OnTxAccepted: func(*chainhash.Hash, btcutil.Amount) {
					t.Error("invalid amount reached nonverbose callback")
				},
				OnTxAcceptedVerbose: func(*btcjson.TxRawResult) {
					t.Error("invalid amount reached verbose callback")
				},
			}}
			client.handleMessage([]byte(fmt.Sprintf(
				`{"id":null,"method":"txaccepted","params":[%q,%s]}`, txID, value)))
			client.handleMessage([]byte(fmt.Sprintf(
				`{"id":null,"method":"txacceptedverbose","params":[{"txid":%q,"vout":[{"value":%s}]}]}`, txID, value)))
		})
	}
}
