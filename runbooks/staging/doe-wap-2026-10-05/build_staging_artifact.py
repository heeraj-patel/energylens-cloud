#!/usr/bin/env python3
"""Bind the private DOE capture to the local-only staging review packet."""

import argparse
import hashlib
import json
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--staging", action="store_true", required=True)
    parser.add_argument("--capture", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    folder = Path(__file__).resolve().parent
    metadata = json.loads((folder / "capture.json").read_text())
    review = json.loads((folder / "review.json").read_text())
    copy = json.loads((folder / "proposed-copy.json").read_text())
    if copy["approval_status"] != "owner_approved_exact_words_2026-10-05":
        raise SystemExit("exact owner-approved staging copy required")

    source = args.capture.read_bytes()
    digest = hashlib.sha256(source).hexdigest()
    if digest != metadata["source_sha256"] or len(source) != metadata["content_bytes"]:
        raise SystemExit("private source capture does not match packet hash and byte count")
    if review["source_url"] != metadata["source_url"] or review["content_type"] != metadata["content_type"]:
        raise SystemExit("source metadata differs from review")
    for field, locator in review["evidence"].items():
        if not locator or locator.encode("utf-8") not in source:
            raise SystemExit(f"evidence locator absent from captured bytes: {field}")
    review["title"] = copy["title"]
    review["benefit_display"] = copy["benefit_display"]
    review["source_sha256"] = digest
    args.output.write_text(json.dumps(review, indent=2) + "\n")
    print(f"STAGING ONLY artifact: {args.output} (source_sha256={digest})")


if __name__ == "__main__":
    main()
