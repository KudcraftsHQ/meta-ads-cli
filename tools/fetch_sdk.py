#!/usr/bin/env python3
"""Download the Facebook Business SDK for Python into a local directory.

The SDK is the source of truth for the Marketing API surface: every endpoint,
HTTP method and parameter type we expose is read back out of it by
gen_command_tree.py. Nothing from this download ships in the binary.
"""

import argparse
import io
import os
import shutil
import sys
import tempfile
import zipfile
from urllib.request import urlopen

REPO = "facebook/facebook-python-business-sdk"


def sdk_zip_url(ref: str) -> str:
    return f"https://codeload.github.com/{REPO}/zip/refs/heads/{ref}"


def fetch(out_dir: str, ref: str) -> str:
    url = sdk_zip_url(ref)
    print(f"fetching {url}", file=sys.stderr)
    with urlopen(url) as resp:
        payload = resp.read()

    tmp = tempfile.mkdtemp(prefix="fb-sdk-")
    try:
        with zipfile.ZipFile(io.BytesIO(payload)) as zf:
            zf.extractall(tmp)
        src = os.path.join(tmp, f"facebook-python-business-sdk-{ref}", "facebook_business")
        if not os.path.isdir(src):
            raise SystemExit(f"unexpected SDK layout, no facebook_business under {src}")
        dst = os.path.join(out_dir, "facebook_business")
        if os.path.exists(dst):
            shutil.rmtree(dst)
        os.makedirs(out_dir, exist_ok=True)
        shutil.copytree(src, dst)
        return dst
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", required=True, help="directory to place facebook_business/ into")
    ap.add_argument("--ref", default="main", help="git branch of the SDK to download")
    args = ap.parse_args()

    dst = fetch(args.out, args.ref)
    print(f"SDK at {dst}", file=sys.stderr)


if __name__ == "__main__":
    main()
