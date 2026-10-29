"""Redaction rules applied to every log line BEFORE it is stored or served.

Order matters: credentials first, then tokens, then addresses, then network/host details.
Keep rules conservative: it is fine to over-redact a synthetic prompt, never fine to leak a key.
"""
import re

_MASK = "***"

RULES = [
    # key=value / key: value credentials (rpcpass=..., password: ..., --rpcpass=..., HF_TOKEN=...)
    # Authorization headers first, so the generic key rule below does not eat only the scheme word
    (re.compile(r"(?i)\b(authorization\s*[=:]\s*)([A-Za-z]+\s+)?[^\s\"'',}\]]+"), lambda m: m.group(1) + (m.group(2) or "") + _MASK),
    (re.compile(r"(?i)\b(--?)?(rpcpass(word)?|rpcuser|password|passphrase|secret|api[_-]?key|token|hf_token)"
                r"(\s*\"?\s*[=:]\s*\"?)([^\s\"'',}\]]+)"), lambda m: f"{m.group(1) or ''}{m.group(2)}{m.group(4)}{_MASK}"),
    # Basic auth inside URLs: http://user:pass@host
    (re.compile(r"(https?://)([^/\s:@]+):([^/\s@]+)@"), r"\1***:***@"),
    # Hugging Face / GitHub / generic bearer tokens
    (re.compile(r"\bhf_[A-Za-z0-9]{16,}\b"), "hf_" + _MASK),
    (re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}\b"), "gh_" + _MASK),
    (re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}"), "Bearer " + _MASK),
    # Seeds / mnemonics: a run of 12+ lowercase words is almost certainly a BIP39 phrase
    (re.compile(r"\b(?:[a-z]{3,8}\s+){11,}[a-z]{3,8}\b"), "[seed phrase redacted]"),
    # Hex private keys (64 hex) and WIF-looking keys
    (re.compile(r"\b[0-9a-fA-F]{64}\b"), lambda m: m.group(0)[:6] + "…" + m.group(0)[-4:]),
    # Pearl bech32 addresses: keep a recognisable prefix/suffix only
    (re.compile(r"\b(prl1[a-z0-9]{5})[a-z0-9]{12,}([a-z0-9]{4})\b"), r"\1…\2"),
    # IPv4 (keep first octet), IPv6
    (re.compile(r"\b(\d{1,3})\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b"), r"\1.x.x.x\2"),
    # IPv6: 4+ hextets, or a :: compressed form. 3-group HH:MM:SS timestamps never match.
    (re.compile(r"(?<![\w:])(?:[0-9a-fA-F]{1,4}:){4,7}[0-9a-fA-F]{1,4}\b"), "[ipv6]"),
    (re.compile(r"(?<![\w:])[0-9a-fA-F]{0,4}::(?:[0-9a-fA-F]{1,4}:){0,5}[0-9a-fA-F]{1,4}\b"), "[ipv6]"),
    # Home directories reveal usernames
    (re.compile(r"/(?:home|Users)/[^/\s]+"), "/~"),
    # Vast.ai / cloud identifiers occasionally printed by tooling
    (re.compile(r"(?i)\b(instance|machine|host)[_ ]?id\s*[=:]\s*\S+"), r"\1_id=***"),
]

# Lines matching any of these are dropped entirely (noise or request payloads).
DROP = [
    re.compile(r"Received request "),
    re.compile(r'"(GET|POST) /(health|metrics|v1/completions|v1/models)'),
    re.compile(r"Template unchanged \(age"),
]

# Loopback is never sensitive and masking it makes logs confusing. Restore after the IPv4 rule.
_LOOPBACK = re.compile(r"\b127\.x\.x\.x\b")


def redact(line: str) -> str:
    for pat, rep in RULES:
        line = pat.sub(rep, line)
    return _LOOPBACK.sub("127.0.0.1", line)


def should_drop(line: str) -> bool:
    return any(p.search(line) for p in DROP)
