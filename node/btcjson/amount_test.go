// Copyright (c) 2025-2026 The Pearl Research Labs
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package btcjson_test

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/pearl-research-labs/pearl/node/btcjson"
)

func TestAmountJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		amount btcjson.Amount
		json   string
	}{
		{0, "0.00000000"},
		{1, "0.00000001"},
		{-1, "-0.00000001"},
		{100000000, "1.00000000"},
		{-125000000, "-1.25000000"},
		{6710886400000002, "67108864.00000002"},
		{6710886400000019, "67108864.00000019"},
		{-6710886400000002, "-67108864.00000002"},
		{9007199254740993, "90071992.54740993"},
		{2100000000000000000, "21000000000.00000000"},
		{math.MaxInt64, "92233720368.54775807"},
		{math.MinInt64, "-92233720368.54775808"},
	}
	for _, test := range tests {
		t.Run(test.json, func(t *testing.T) {
			encoded, err := json.Marshal(test.amount)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.json {
				t.Fatalf("got %s, want %s", encoded, test.json)
			}
			var decoded btcjson.Amount
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded != test.amount {
				t.Fatalf("got %d grains, want %d", decoded, test.amount)
			}
		})
	}
}

func TestAmountUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		json string
		want btcjson.Amount
	}{
		{"0", 0},
		{"-0.0", 0},
		{" \t\r\n1.25\n", 125000000},
		{"1e-8", 1},
		{"-1E-8", -1},
		{"1e+1", 1000000000},
		{"1.000000000", 100000000},
		{"0.0000000100", 1},
		{"100000000000000000000e-28", 1},
		{"0.000000000000000000001e21", 100000000},
		{"6.710886400000002e7", 6710886400000002},
		{"9223372036854775807e-8", math.MaxInt64},
		{"-9223372036854775808e-8", math.MinInt64},
		{"1000000000e-17", 1},
		{"0e" + strings.Repeat("9", 4096), 0},
		{"-0.000e-" + strings.Repeat("9", 4096), 0},
		{"1e" + strings.Repeat("0", 4096), 100000000},
		{"1" + strings.Repeat("0", 4096) + "e-4104", 1},
	}
	for _, test := range tests {
		var got btcjson.Amount
		if err := json.Unmarshal([]byte(test.json), &got); err != nil {
			t.Errorf("Unmarshal %.80q: %v", test.json, err)
			continue
		}
		if got != test.want {
			t.Errorf("Unmarshal %.80q = %d grains, want %d", test.json, got, test.want)
		}
	}
}

func TestAmountUnmarshalJSONErrors(t *testing.T) {
	t.Parallel()

	for _, number := range []string{
		"", " ", "null", "true", "false", `"1.25"`, `"1"`, "[]", "{}",
		"+1", "01", ".1", "1.", "--1", "NaN", "Infinity", "1e", "1 2", "\u00a01",
		"0.000000001", "-0.000000001", "1e-9", "1.000000001",
		"92233720368.54775808", "-92233720368.54775809", "100000000000",
		"123456789012345678901e-9", "1e9223372036854775807", "1e-9223372036854775808",
		"1e" + strings.Repeat("9", 4096), "1e-" + strings.Repeat("9", 4096),
	} {
		got := btcjson.Amount(123)
		if err := got.UnmarshalJSON([]byte(number)); err == nil {
			t.Errorf("Unmarshal %.80q unexpectedly succeeded", number)
		}
		if got != 123 {
			t.Errorf("Unmarshal %.80q changed receiver on error to %d", number, got)
		}
	}
}

func TestAmountResultRoundTrip(t *testing.T) {
	t.Parallel()

	const amount btcjson.Amount = 6710886400000002
	for _, result := range []interface{}{
		btcjson.Vout{Value: amount},
		btcjson.PrevOut{Value: amount},
		btcjson.GetTxOutResult{Value: amount},
	} {
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"value":67108864.00000002`) {
			t.Errorf("%T has inexact JSON: %s", result, encoded)
		}
		decoded := reflect.New(reflect.TypeOf(result))
		if err := json.Unmarshal(encoded, decoded.Interface()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.Elem().Interface(), result) {
			t.Errorf("%T changed after JSON round trip", result)
		}
	}
}

func TestAmountNotificationRoundTrip(t *testing.T) {
	t.Parallel()

	const amount btcjson.Amount = 6710886400000002
	for _, arg := range []interface{}{amount, "67108864.00000002", "6.710886400000002e7"} {
		command, err := btcjson.NewCmd("txaccepted", "123", arg)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(command, btcjson.NewTxAcceptedNtfn("123", amount)) {
			t.Fatalf("NewCmd(%v) = %#v", arg, command)
		}
		encoded, err := btcjson.MarshalCmd(btcjson.RpcVersion1, nil, command)
		if err != nil {
			t.Fatal(err)
		}
		var request btcjson.Request
		if err := json.Unmarshal(encoded, &request); err != nil {
			t.Fatal(err)
		}
		if string(request.Params[1]) != "67108864.00000002" {
			t.Fatalf("request rounded amount: %s", request.Params[1])
		}
		decoded, err := btcjson.UnmarshalCmd(&request)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, command) {
			t.Fatalf("notification round trip = %#v, want %#v", decoded, command)
		}
	}
	for _, arg := range []interface{}{1.25, "1e-9", `"1.25"`} {
		if _, err := btcjson.NewCmd("txaccepted", "123", arg); err == nil {
			t.Errorf("NewCmd unexpectedly accepted %v (%T)", arg, arg)
		}
	}
}

func TestRequestPreservesParameterJSON(t *testing.T) {
	t.Parallel()

	input := `{"jsonrpc":"1.0","method":"test","params":[67108864.00000002,1e-8,{"amount":9007199254740993},null,true,"text"],"id":1}`
	var request btcjson.Request
	if err := json.Unmarshal([]byte(input), &request); err != nil {
		t.Fatal(err)
	}
	want := []string{"67108864.00000002", "1e-8", `{"amount":9007199254740993}`, "null", "true", `"text"`}
	if len(request.Params) != len(want) {
		t.Fatalf("got %d params, want %d", len(request.Params), len(want))
	}
	for i, param := range request.Params {
		if string(param) != want[i] {
			t.Errorf("param %d = %s, want %s", i, param, want[i])
		}
	}
	for _, input := range []string{`{"method":"test"}`, `{"method":"test","params":null}`, `{"method":"test","params":[]}`} {
		if err := json.Unmarshal([]byte(input), &request); err != nil {
			t.Fatal(err)
		}
		if request.Params == nil || len(request.Params) != 0 {
			t.Errorf("missing or null params should decode to an empty array: %s", input)
		}
	}
	for _, input := range []string{"null", `{"params":{}}`, `{"params":1}`, `{"params":"x"}`} {
		if err := json.Unmarshal([]byte(input), &request); err == nil {
			t.Errorf("invalid request unexpectedly accepted: %s", input)
		}
	}
}

func TestAmountHelp(t *testing.T) {
	t.Parallel()

	examples, complex := btcjson.TstReflectTypeToJSONExample(func(key string) string {
		return key
	}, reflect.TypeOf(btcjson.Amount(0)), 0, "amount")
	if complex || !reflect.DeepEqual(examples, []string{"n.nnn"}) {
		t.Fatalf("amount JSON example = %v, complex=%v", examples, complex)
	}
	field := reflect.TypeOf(struct{ Vout btcjson.Vout }{}).Field(0)
	if usage := btcjson.TstFieldUsage(field, nil); !strings.Contains(usage, `"value":n.nnn`) {
		t.Fatalf("amount usage should show a decimal number: %s", usage)
	}
}

func FuzzAmountRoundTrip(f *testing.F) {
	for _, amount := range []int64{0, 1, -1, 6710886400000002, 9007199254740993, math.MinInt64, math.MaxInt64} {
		f.Add(amount)
	}
	f.Fuzz(func(t *testing.T, grains int64) {
		encoded, err := json.Marshal(btcjson.Amount(grains))
		if err != nil {
			t.Fatal(err)
		}
		var decoded btcjson.Amount
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if int64(decoded) != grains {
			t.Fatalf("%d grains became %d through %s", grains, decoded, encoded)
		}
	})
}
