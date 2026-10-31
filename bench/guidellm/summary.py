"""Contender x profile summary table from GuideLLM JSON.

Reads <results>/<contender>/<shape>/<profile>.json plus each contender's
manifest.jsonl, and writes summary.md and summary.csv into <results>.
SLO attainment is computed per request against the frozen slo.json copy
stored in <results>/env/<contender>/ (the share of attempted requests that
met both the TTFT and ITL target for their shape).
"""
from __future__ import annotations

import csv
import json
from pathlib import Path

COLUMNS = ["contender", "shape", "profile", "strategy", "ok", "err", "err_rate",
           "ttft_p50", "ttft_p95", "ttft_p99", "itl_p50", "itl_p95",
           "e2e_p50", "e2e_p95", "e2e_p99", "req_s", "out_tok_s", "slo_attain", "slo_pass"]


def pct(metric: dict, key: str) -> float | None:
    block = metric.get("successful") or {}
    if not block.get("count"):
        return None
    return block["percentiles"].get(key)


def mean(metric: dict) -> float | None:
    block = metric.get("successful") or {}
    return block.get("mean") if block.get("count") else None


def attainment(bench: dict, slo: dict) -> float | None:
    reqs = bench["requests"]
    attempted = len(reqs.get("successful") or []) + len(reqs.get("errored") or [])
    if not attempted:
        return None
    met = 0
    for r in reqs.get("successful") or []:
        ttft, itl = r.get("time_to_first_token_ms"), r.get("inter_token_latency_ms")
        if ttft is None or ttft > slo["ttft_ms"]:
            continue
        if itl is not None and itl > slo["itl_ms"]:
            continue
        met += 1
    return met / attempted


def strategy_label(bench: dict) -> str:
    s = bench["config"]["strategy"]
    kind = s.get("type_", "?")
    if kind == "concurrent":
        return f"concurrent@{s.get('streams')}"
    rate = s.get("rate")
    return f"{kind}@{rate:.3g}" if isinstance(rate, (int, float)) else kind


def rows_for(path: Path, contender: str, shape: str, profile: str,
             slo_cfg: dict | None) -> list[dict]:
    doc = json.loads(path.read_text())
    rows = []
    for bench in doc["benchmarks"]:
        m = bench["metrics"]
        ok = len(bench["requests"].get("successful") or [])
        err = len(bench["requests"].get("errored") or [])
        slo = slo_cfg["shapes"].get(shape) if slo_cfg else None
        att = attainment(bench, slo) if slo else None
        rows.append({
            "contender": contender, "shape": shape, "profile": profile,
            "strategy": strategy_label(bench), "ok": ok, "err": err,
            "err_rate": err / (ok + err) if ok + err else None,
            "ttft_p50": pct(m["time_to_first_token_ms"], "p50"),
            "ttft_p95": pct(m["time_to_first_token_ms"], "p95"),
            "ttft_p99": pct(m["time_to_first_token_ms"], "p99"),
            "itl_p50": pct(m["inter_token_latency_ms"], "p50"),
            "itl_p95": pct(m["inter_token_latency_ms"], "p95"),
            "e2e_p50": pct(m["request_latency"], "p50"),
            "e2e_p95": pct(m["request_latency"], "p95"),
            "e2e_p99": pct(m["request_latency"], "p99"),
            "req_s": mean(m["requests_per_second"]),
            "out_tok_s": mean(m["output_tokens_per_second"]),
            "slo_attain": att,
            "slo_pass": (None if att is None else att >= slo_cfg["min_attainment"]),
        })
    return rows


def fmt(v) -> str:
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.3f}" if abs(v) < 10 else f"{v:.1f}"
    return str(v)


def table(rows: list[dict]) -> str:
    head = "| " + " | ".join(COLUMNS) + " |\n|" + "|".join("---" for _ in COLUMNS) + "|\n"
    return head + "".join("| " + " | ".join(fmt(r[c]) for c in COLUMNS) + " |\n" for r in rows)


def main(results: Path) -> int:
    results = results.resolve()
    rows: list[dict] = []
    failures: list[dict] = []
    kinds: set[str] = set()
    for manifest in sorted(results.glob("*/manifest.jsonl")):
        contender = manifest.parent.name
        slo_path = results / "env" / contender / "slo.json"
        slo_cfg = json.loads(slo_path.read_text()) if slo_path.exists() else None
        for line in manifest.read_text().splitlines():
            rec = json.loads(line)
            kinds.add(rec.get("run_kind", "UNKNOWN"))
            if rec["stage"] == "warmup":
                continue
            if rec["status"] != "ok":
                failures.append(rec)
            elif rec["stage"] == "measure":
                rows += rows_for(results / rec["json"], contender, rec["shape"],
                                 rec["profile"], slo_cfg)
    banner = " / ".join(sorted(kinds)) or "UNKNOWN"
    out = [f"# GuideLLM summary - {banner}\n\n",
           "Units: TTFT and ITL in ms, e2e in s, rates per second. "
           "slo_attain = share of attempted requests meeting both slo.json targets. "
           "'-' means no data.\n"]
    if banner != "MEASURED":
        out.append(f"\n**{banner}: plumbing validation only. Not a result.**\n")
    for shape in sorted({r["shape"] for r in rows}):
        out.append(f"\n## Shape {shape}\n\n" + table([r for r in rows if r["shape"] == shape]))
    if failures:
        out.append("\n## Failed or skipped runs\n\n")
        out += [f"- {f['contender']} {f['shape']} {f['profile']}: {f['status']} - {f['reason']}\n"
                for f in failures]
    (results / "summary.md").write_text("".join(out))
    with (results / "summary.csv").open("w", newline="") as fh:
        w = csv.DictWriter(fh, fieldnames=COLUMNS)
        w.writeheader()
        w.writerows(rows)
    print("".join(out))
    return 0
