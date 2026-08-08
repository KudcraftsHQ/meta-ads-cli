#!/usr/bin/env python3
"""Generate the meta-ads command tree from the Facebook Business SDK for Python.

The SDK's `adobjects/*.py` modules are themselves generated from Meta's internal
API specs, which makes them the most complete public description of the
Marketing API surface. Every request-issuing method looks like this:

    def create_campaign(self, fields=None, params=None, ...):
        param_types = {'name': 'string', 'objective': 'objective_enum', ...}
        enums = {'objective_enum': Campaign.Objective.__dict__.values(), ...}
        request = FacebookRequest(
            node_id=self['id'], method='POST', endpoint='/campaigns',
            api_type='EDGE', ...)

So we walk the AST, pull the `FacebookRequest(...)` keyword arguments, the
`param_types` dict and the `enums` dict out of each such method, resolve enum
references against a registry of every inner class in the SDK, and emit JSON.

Output (into --out):

    meta.json           api version and graph URLs -- tiny, read on every call
    index.json          resource and op names -- read only by list/tree/completion
    resources/<r>.json  one file per resource with full parameter detail

The Go binary embeds all three. Dispatching `meta-ads ad-account get` looks the
resource up in the embedded filesystem by name and unmarshals that one file, so
the 200 KB index is never touched on the hot path. Nothing here runs at CLI
runtime.
"""

import argparse
import ast
import json
import os
import re
import shutil
import sys
from typing import Any, Dict, List, Optional, Tuple

# Methods on AbstractCrudObject that exist on every class; we give them short
# names so `meta-ads campaign get` reads better than `meta-ads campaign api-get`.
CANONICAL_OPS = {
    "api_get": "get",
    "api_create": "create",
    "api_update": "update",
    "api_delete": "delete",
}

# Base kinds the Go side knows how to coerce a flag value into.
KIND_STRING = "string"
KIND_INT = "int"
KIND_FLOAT = "float"
KIND_BOOL = "bool"
KIND_JSON = "json"
KIND_LIST = "list"
KIND_ENUM = "enum"
KIND_FILE = "file"
KIND_DATETIME = "datetime"

_INT_TYPES = {"int", "unsigned int", "int64", "unsigned int64", "long", "number"}
_FLOAT_TYPES = {"float", "double"}
_BOOL_TYPES = {"bool", "boolean"}
_STRING_TYPES = {"string", "str"}
_FILE_TYPES = {"file"}

# SDK plumbing rather than API resources -- they carry inherited request methods
# but there is no such node in the Graph API.
SKIP_CLASSES = {"AbstractObject", "AbstractCrudObject"}


def to_kebab(name: str) -> str:
    """AdAccount -> ad-account, AdsInsights -> ads-insights, IGUser -> ig-user."""
    s = re.sub(r"(.)([A-Z][a-z]+)", r"\1-\2", name)
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1-\2", s)
    return s.replace("_", "-").lower()


def read_api_version(sdk_root: str) -> str:
    path = os.path.join(sdk_root, "apiconfig.py")
    with open(path, "r", encoding="utf-8") as f:
        content = f.read()
    m = re.search(r"'API_VERSION'\s*:\s*'([^']+)'", content)
    if not m:
        raise SystemExit("could not find API_VERSION in apiconfig.py")
    return m.group(1)


# --------------------------------------------------------------------------
# Pass 1: index every inner class in the SDK so enum references can be resolved
# --------------------------------------------------------------------------


def index_inner_classes(module: ast.Module, registry: Dict[Tuple[str, str], List[str]],
                        fields: Dict[str, List[str]]) -> None:
    """Record Outer.Inner -> [string constants] for every nested class.

    Also records the `Field` inner class separately: those are the field names
    accepted by --fields, which is worth surfacing in `describe`.
    """
    for node in module.body:
        if not isinstance(node, ast.ClassDef):
            continue
        for item in node.body:
            if not isinstance(item, ast.ClassDef):
                continue
            values: List[str] = []
            for stmt in item.body:
                if not isinstance(stmt, ast.Assign):
                    continue
                if not isinstance(stmt.value, ast.Constant) or not isinstance(stmt.value.value, str):
                    continue
                for target in stmt.targets:
                    if isinstance(target, ast.Name):
                        values.append(stmt.value.value)
            if not values:
                continue
            registry[(node.name, item.name)] = sorted(set(values))
            if item.name == "Field":
                fields[node.name] = sorted(set(values))


# --------------------------------------------------------------------------
# Pass 2: extract operations
# --------------------------------------------------------------------------


def _dict_of_strings(node: ast.AST) -> Dict[str, str]:
    out: Dict[str, str] = {}
    if not isinstance(node, ast.Dict):
        return out
    for k, v in zip(node.keys, node.values):
        if isinstance(k, ast.Constant) and isinstance(v, ast.Constant):
            if isinstance(k.value, str) and isinstance(v.value, str):
                out[k.value] = v.value
    return out


def _assigned_dict(func: ast.FunctionDef, name: str) -> Optional[ast.Dict]:
    for stmt in func.body:
        if isinstance(stmt, ast.Assign):
            for target in stmt.targets:
                if isinstance(target, ast.Name) and target.id == name:
                    if isinstance(stmt.value, ast.Dict):
                        return stmt.value
    return None


def _attr_chain(node: ast.AST) -> List[str]:
    """Flatten Campaign.Objective.__dict__.values -> ['Campaign','Objective','__dict__','values']."""
    parts: List[str] = []
    cur = node
    while isinstance(cur, ast.Attribute):
        parts.append(cur.attr)
        cur = cur.value
    if isinstance(cur, ast.Name):
        parts.append(cur.id)
    parts.reverse()
    return parts


def extract_enums(func: ast.FunctionDef, class_name: str,
                  registry: Dict[Tuple[str, str], List[str]]) -> Dict[str, List[str]]:
    """Resolve the `enums = {...}` dict into concrete value lists."""
    node = _assigned_dict(func, "enums")
    if node is None:
        return {}
    resolved: Dict[str, List[str]] = {}
    for k, v in zip(node.keys, node.values):
        if not isinstance(k, ast.Constant) or not isinstance(k.value, str):
            continue
        target = v.func if isinstance(v, ast.Call) else v
        parts = _attr_chain(target)
        # Campaign.Objective.__dict__.values -> owner=Campaign, inner=Objective
        parts = [p for p in parts if p not in ("__dict__", "values", "keys")]
        if len(parts) >= 2:
            owner, inner = parts[-2], parts[-1]
        elif len(parts) == 1:
            owner, inner = class_name, parts[0]
        else:
            continue
        if owner == "self":
            owner = class_name
        values = registry.get((owner, inner))
        if values:
            resolved[k.value] = values
    return resolved


def extract_request(func: ast.FunctionDef) -> Optional[Dict[str, Any]]:
    """Pull the keyword arguments of the FacebookRequest(...) construction."""
    for node in ast.walk(func):
        if not isinstance(node, ast.Call):
            continue
        if not isinstance(node.func, ast.Name) or node.func.id != "FacebookRequest":
            continue
        args: Dict[str, Any] = {}
        for kw in node.keywords:
            if kw.arg and isinstance(kw.value, ast.Constant):
                args[kw.arg] = kw.value.value
        return args
    return None


def classify(param_type: str, enums: Dict[str, List[str]]) -> Dict[str, Any]:
    """Map an SDK type string onto a kind the Go flag layer can coerce into."""
    raw = param_type.strip()

    inner = None
    m = re.fullmatch(r"[Ll]ist<(.+)>", raw)
    if m:
        inner = m.group(1).strip()

    def base_kind(t: str) -> str:
        low = t.lower()
        if t in enums or low.endswith("_enum"):
            return KIND_ENUM
        if low in _STRING_TYPES:
            return KIND_STRING
        if low in _INT_TYPES:
            return KIND_INT
        if low in _FLOAT_TYPES:
            return KIND_FLOAT
        if low in _BOOL_TYPES:
            return KIND_BOOL
        if low in _FILE_TYPES:
            return KIND_FILE
        if low == "datetime":
            return KIND_DATETIME
        # Object, map<string, X>, and every named SDK struct go over the wire
        # as JSON, so the flag takes a JSON literal.
        return KIND_JSON

    out: Dict[str, Any] = {"type": raw}
    if inner is not None:
        out["kind"] = KIND_LIST
        out["item"] = base_kind(inner)
        if out["item"] == KIND_ENUM:
            vals = enums.get(inner)
            if vals:
                out["enum"] = vals
    else:
        out["kind"] = base_kind(raw)
        if out["kind"] == KIND_ENUM:
            vals = enums.get(raw)
            if vals:
                out["enum"] = vals
    return out


def op_name(method_name: str) -> str:
    if method_name in CANONICAL_OPS:
        return CANONICAL_OPS[method_name]
    return method_name.replace("_", "-")


def collect_operations(module: ast.Module, registry: Dict[Tuple[str, str], List[str]]) -> Dict[str, List[Dict[str, Any]]]:
    by_class: Dict[str, List[Dict[str, Any]]] = {}
    for node in module.body:
        if not isinstance(node, ast.ClassDef):
            continue
        ops: List[Dict[str, Any]] = []
        seen: set = set()
        for item in node.body:
            if not isinstance(item, ast.FunctionDef):
                continue
            if item.name.startswith("_"):
                continue
            req = extract_request(item)
            if not req or not req.get("method"):
                continue
            enums = extract_enums(item, node.name, registry)
            param_types = _dict_of_strings(_assigned_dict(item, "param_types") or ast.Dict(keys=[], values=[]))

            params = []
            for pname in sorted(param_types):
                info = classify(param_types[pname], enums)
                params.append({
                    "name": pname,
                    "flag": pname.replace("_", "-"),
                    **info,
                })

            name = op_name(item.name)
            if name in seen:
                continue
            seen.add(name)
            ops.append({
                "name": name,
                "method": str(req.get("method", "GET")).upper(),
                "endpoint": req.get("endpoint", "/"),
                "api_type": req.get("api_type", "NODE"),
                "allow_file_upload": bool(req.get("allow_file_upload", False)),
                "params": params,
            })
        if ops:
            ops.sort(key=lambda o: o["name"])
            by_class[node.name] = ops
    return by_class


# --------------------------------------------------------------------------


def build(sdk_root: str) -> Tuple[Dict[str, Any], List[Dict[str, Any]]]:
    adobjects = os.path.join(sdk_root, "adobjects")
    if not os.path.isdir(adobjects):
        raise SystemExit(f"no adobjects directory under {sdk_root}")

    modules: List[Tuple[str, ast.Module]] = []
    for fname in sorted(os.listdir(adobjects)):
        if not fname.endswith(".py") or fname == "__init__.py":
            continue
        path = os.path.join(adobjects, fname)
        with open(path, "r", encoding="utf-8") as f:
            src = f.read()
        try:
            modules.append((fname, ast.parse(src)))
        except SyntaxError as exc:
            print(f"skipping {fname}: {exc}", file=sys.stderr)

    registry: Dict[Tuple[str, str], List[str]] = {}
    class_fields: Dict[str, List[str]] = {}
    for _, module in modules:
        index_inner_classes(module, registry, class_fields)

    resources: List[Dict[str, Any]] = []
    for _, module in modules:
        for class_name, ops in collect_operations(module, registry).items():
            resources.append({
                "name": to_kebab(class_name),
                "class": class_name,
                "fields": class_fields.get(class_name, []),
                "ops": ops,
            })

    # Two SDK modules can define the same class name; keep the richer one.
    merged: Dict[str, Dict[str, Any]] = {}
    for r in resources:
        if r["class"] in SKIP_CLASSES:
            continue
        prev = merged.get(r["name"])
        if prev is None or len(r["ops"]) > len(prev["ops"]):
            merged[r["name"]] = r
    resources = sorted(merged.values(), key=lambda r: r["name"])

    index = {
        "resources": [
            {
                "name": r["name"],
                "class": r["class"],
                "ops": [{"name": o["name"], "method": o["method"], "endpoint": o["endpoint"],
                         "api_type": o["api_type"], "params": len(o["params"]),
                         "allow_file_upload": o["allow_file_upload"]}
                        for o in r["ops"]],
            }
            for r in resources
        ],
    }
    meta = {
        "version": 2,
        "api_version": read_api_version(sdk_root),
        "graph_url": "https://graph.facebook.com",
        "graph_video_url": "https://graph-video.facebook.com",
    }
    return meta, index, resources


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--sdk", required=True, help="directory containing facebook_business/")
    ap.add_argument("--out", required=True, help="schemas directory to write into")
    args = ap.parse_args()

    sdk_root = os.path.join(args.sdk, "facebook_business")
    if not os.path.isdir(sdk_root):
        sdk_root = args.sdk

    meta, index, resources = build(sdk_root)

    res_dir = os.path.join(args.out, "resources")
    if os.path.isdir(res_dir):
        shutil.rmtree(res_dir)
    os.makedirs(res_dir, exist_ok=True)

    for r in resources:
        with open(os.path.join(res_dir, f"{r['name']}.json"), "w", encoding="utf-8") as f:
            json.dump(r, f, separators=(",", ":"), sort_keys=False)
            f.write("\n")

    for name, payload in (("index.json", index), ("meta.json", meta)):
        with open(os.path.join(args.out, name), "w", encoding="utf-8") as f:
            json.dump(payload, f, separators=(",", ":"), sort_keys=False)
            f.write("\n")

    ops = sum(len(r["ops"]) for r in resources)
    params = sum(len(o["params"]) for r in resources for o in r["ops"])
    print(f"api {meta['api_version']}: {len(resources)} resources, {ops} ops, {params} params",
          file=sys.stderr)


if __name__ == "__main__":
    main()
