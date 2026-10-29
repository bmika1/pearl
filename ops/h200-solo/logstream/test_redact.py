import unittest

from redact import redact, should_drop


class RedactTests(unittest.TestCase):
    def test_credentials(self):
        self.assertEqual(redact("--rpcpass=hunter2 --rpcuser=bob"), "--rpcpass=*** --rpcuser=***")
        self.assertIn("password=***", redact("password=abc123"))
        self.assertIn("HF_TOKEN=***", redact("HF_TOKEN=hf_abcdefghijklmnopqrstuvwxyz"))
        self.assertNotIn("hf_abcdefghijklmnopqrstuvwxyz", redact("token hf_abcdefghijklmnopqrstuvwxyz"))
        self.assertEqual(redact("url http://u:p@host:44107/x"), "url http://***:***@host:44107/x")
        self.assertIn("Bearer ***", redact("Authorization: Bearer abcdefgh1234"))

    def test_seed_and_keys(self):
        seed = "abandon ability able about above absent absorb abstract absurd abuse access accident"
        self.assertIn("[seed phrase redacted]", redact("seed: " + seed))
        key = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        out = redact("priv " + key)
        self.assertNotIn(key, out)
        self.assertIn("e3b0c4…b855", out)

    def test_address_and_network(self):
        addr = "prl1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr"
        out = redact("miningaddr " + addr)
        self.assertNotIn(addr, out)
        self.assertTrue(out.startswith("miningaddr prl1p5cyx…"))
        self.assertEqual(redact("peer 203.0.113.9:44108 connected"), "peer 203.x.x.x:44108 connected")
        self.assertEqual(redact("rpc 127.0.0.1:44107 up"), "rpc 127.0.0.1:44107 up")
        self.assertEqual(redact("file /home/bartek/x.log and /Users/bob/y"), "file /~/x.log and /~/y")

    def test_drop(self):
        self.assertTrue(should_drop('INFO: 127.0.0.1 - "POST /v1/completions HTTP/1.1" 200'))
        self.assertTrue(should_drop("Received request cmpl-1: prompt: 'x'"))
        self.assertFalse(should_drop("Block found! header=..."))

    def test_json_structure_survives(self):
        import json
        raw = '{"leak":"rpcpass=hunter2","token":"abc","x":{"password": "p w"}}'
        out = json.loads(redact(raw))
        self.assertEqual(out["leak"], "rpcpass=***")
        self.assertEqual(out["token"], "***")
        self.assertTrue(out["x"]["password"].startswith("***") and "p w" not in out["x"]["password"])

    def test_passthrough(self):
        line = "2026-10-28 20:00:00 - INFO - miner work rate: 731.20 TH/s - gemm_operators.py:140"
        self.assertEqual(redact(line), line)


if __name__ == "__main__":
    unittest.main()
