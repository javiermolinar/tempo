#!/usr/bin/env python3
"""Producer pipeline: tempo-cli `experimental trace-diff` -> traces-microbench candidates.

This script lives in tempo.fork (the repo being optimized), NOT in the bench.
Per traces-microbench AGENTS.md, the bench is a pure function over candidate
files and never invokes the producer. This script IS the producer pipeline: it
runs the experimental trace-diff command over the bench's diff_vs_raw corpus,
adapts the `trace-patch-v0` output into the bench's `ChangeList` schema, and
drops the candidate files the bench then grades deterministically.

Per pair under <bench>/dataset/diff_vs_raw/<pair>/:
  1. tempo-cli experimental trace-diff
        --trace-a <pair>/trace_a_otel.json
        --trace-b <pair>/trace_b_otel.json
        --format trace-patch-v0
     (the *_otel.json files are produced by the bench's generate step via
      canonical_trace_to_otel_json; they are Tempo/OTEL protobuf JSON, which
      is what tempo-cli reads. The bench-canonical trace_a.json is NOT.)
  2. adapt trace-patch-v0 -> ChangeList
  3. write <bench>/candidates/diff_vs_raw/<run>/<pair>/json_patch.json + meta.json

Then, from the bench repo:
  uv run traces-microbench score --experiment diff_vs_raw --run <run>
  jq . results/diff_vs_raw/<run>/aggregate.json

Adapter mapping (trace-patch-v0 -> ChangeList):
  modified[].changes field  duration_ms  before<after  -> latency_increase
  modified[].changes field  duration_ms  before>after  -> latency_decrease
  modified[].changes field  status       ok->error     -> error_added
  modified[].changes field  status       error->ok     -> error_removed
  modified[].changes attribute (add/remove/modify)      -> attribute_changed (key)
  added[]                                                -> span_added
  removed[]                                              -> span_removed
  span position_in_parent = span.path[-1]   (0 for roots/empty path)

Known producer gaps the bench will surface (not handled here, by design):
  - trace-patch-v0 diffs span attributes only; resource-attribute and event
    changes are not emitted, so the bench will score them as recall misses.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent


# ---------------------------------------------------------------------------
# Adapter: trace-patch-v0 -> bench ChangeList (pure function, no I/O).
# ---------------------------------------------------------------------------


def _position_in_parent(path: list | None) -> int:
    """Bench `position_in_parent` is the last sibling index of the span path."""
    if not path:
        return 0
    return int(path[-1])


def _status_is_error(value) -> bool:
    return str(value).lower() == "error"


def _span_ref(span: dict) -> dict:
    return {
        "service": span.get("service", ""),
        "name": span.get("name", ""),
        "position_in_parent": _position_in_parent(span.get("path")),
    }


def adapt_trace_patch_v0(patch: dict) -> dict:
    """Convert a trace-patch-v0 Result dict into a bench ChangeList dict.

    Mechanical -> semantic: tempo-cli emits op=modify on duration_ms/status;
    the bench taxonomy wants directional kinds, derived here from before/after.
    """
    changes: list[dict] = []

    for mod in patch.get("modified") or []:
        ref = _span_ref(mod.get("span") or {})
        for ch in mod.get("changes") or []:
            target = ch.get("target") or {}
            ttype = target.get("type")
            before = ch.get("before")
            after = ch.get("after")

            if ttype == "field" and target.get("name") == "duration_ms":
                b = int(before) if before is not None else None
                a = int(after) if after is not None else None
                if b is None or a is None or a == b:
                    continue
                delta = a - b
                pct = (delta / b * 100.0) if b else 0.0
                changes.append({
                    "kind": "latency_increase" if delta > 0 else "latency_decrease",
                    "span_ref": ref,
                    "before_ms": b,
                    "after_ms": a,
                    "delta_ms": delta,
                    "delta_pct": pct,
                })

            elif ttype == "field" and target.get("name") == "status":
                before_err = _status_is_error(before)
                after_err = _status_is_error(after)
                if after_err and not before_err:
                    changes.append({
                        "kind": "error_added", "span_ref": ref,
                        "status_before": str(before), "status_after": str(after),
                    })
                elif before_err and not after_err:
                    changes.append({
                        "kind": "error_removed", "span_ref": ref,
                        "status_before": str(before), "status_after": str(after),
                    })
                # ok<->unset and other transitions have no taxonomy entry; skip.

            elif ttype == "attribute":
                key = target.get("key")
                if not key:
                    continue
                changes.append({
                    "kind": "attribute_changed", "span_ref": ref,
                    "key": key, "before": before, "after": after,
                })
            # field changes other than duration_ms/status: no taxonomy entry.

    for add in patch.get("added") or []:
        changes.append({"kind": "span_added", "span_ref": _span_ref(add.get("span") or {})})

    for rem in patch.get("removed") or []:
        changes.append({"kind": "span_removed", "span_ref": _span_ref(rem.get("span") or {})})

    return {"changes": changes}


# ---------------------------------------------------------------------------
# Runner.
# ---------------------------------------------------------------------------


def _producer_id() -> str:
    try:
        sha = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=SCRIPT_DIR, check=True, capture_output=True, text=True,
        ).stdout.strip()
        return f"tempo-cli@{sha}"
    except Exception:
        return "tempo-cli@unknown"


def _run_diff(tempo_cli: str, fmt: str, trace_a: Path, trace_b: Path, out: Path) -> float:
    cmd = [
        tempo_cli, "experimental", "trace-diff",
        "--trace-a", str(trace_a),
        "--trace-b", str(trace_b),
        "--format", fmt,
        "-o", str(out),
    ]
    t0 = time.perf_counter()
    proc = subprocess.run(cmd, check=False, capture_output=True, text=True)
    producer_ms = (time.perf_counter() - t0) * 1000.0
    if proc.returncode != 0:
        raise RuntimeError(
            f"tempo-cli exit={proc.returncode}\ncmd={' '.join(cmd)}\nstderr:\n{proc.stderr}"
        )
    return producer_ms


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bench-root", type=Path, required=True,
                    help="traces-microbench repo root (contains dataset/diff_vs_raw).")
    ap.add_argument("--run", required=True,
                    help="Run/producer label, e.g. the tempo.fork short sha.")
    ap.add_argument("--tempo-cli", default="tempo-cli",
                    help="Path to the built tempo-cli binary (default: tempo-cli on PATH).")
    ap.add_argument("--format", default="trace-patch-v0", help="trace-diff --format value.")
    ap.add_argument("--only", action="append", default=[],
                    help="Only process these pair ids (repeatable).")
    ap.add_argument("--keep-raw", action="store_true",
                    help="Also write the unadapted trace-patch-v0 output as trace_patch_v0.json.")
    args = ap.parse_args()

    bench_root = args.bench_root.resolve()
    dataset_root = bench_root / "dataset" / "diff_vs_raw"
    if not dataset_root.is_dir():
        print(f"error: no dataset at {dataset_root}; run the bench `generate` first", file=sys.stderr)
        return 2

    cand_root = bench_root / "candidates" / "diff_vs_raw" / args.run
    producer_id = _producer_id()

    pairs = sorted(p.name for p in dataset_root.iterdir() if p.is_dir())
    if args.only:
        wanted = set(args.only)
        missing = sorted(wanted - set(pairs))
        if missing:
            print(f"error: requested pair(s) not found: {missing}", file=sys.stderr)
            return 2
        pairs = [p for p in pairs if p in wanted]
    if not pairs:
        print("error: no pairs selected", file=sys.stderr)
        return 2

    done, skipped = 0, 0
    for pair in pairs:
        ds = dataset_root / pair
        trace_a = ds / "trace_a_otel.json"
        trace_b = ds / "trace_b_otel.json"
        if not (trace_a.exists() and trace_b.exists()):
            print(f"skip {pair}: missing trace_a_otel.json/trace_b_otel.json "
                  f"(regenerate the bench corpus so OTEL fixtures exist)", file=sys.stderr)
            skipped += 1
            continue

        out_dir = cand_root / pair
        out_dir.mkdir(parents=True, exist_ok=True)
        raw_out = out_dir / "trace_patch_v0.json"

        producer_ms = _run_diff(args.tempo_cli, args.format, trace_a, trace_b, raw_out)
        patch = json.loads(raw_out.read_text(encoding="utf-8"))
        change_list = adapt_trace_patch_v0(patch)

        (out_dir / "json_patch.json").write_text(
            json.dumps(change_list, indent=2), encoding="utf-8"
        )
        meta = {
            "producer_id": producer_id,
            "variants": {
                "json_patch": {
                    "producer_ms": producer_ms,
                    "encoding_flag": f"--format {args.format}",
                }
            },
        }
        (out_dir / "meta.json").write_text(json.dumps(meta, indent=2), encoding="utf-8")
        if not args.keep_raw:
            raw_out.unlink(missing_ok=True)

        n = len(change_list["changes"])
        print(f"ok   {pair}: {n} change(s)  ({producer_ms:.1f} ms)  -> {out_dir}")
        done += 1

    print(f"\ndone: {done} pair(s) written under {cand_root}; {skipped} skipped")
    print(f"producer_id: {producer_id}")
    print("\nnext (from the bench repo):")
    print(f"  uv run traces-microbench score --experiment diff_vs_raw --run {args.run}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
