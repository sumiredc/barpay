"""docs/api/edge.yaml, for checking requests and answers against it."""

import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

SPEC_PATH = Path(__file__).resolve().parents[2] / "docs" / "api" / "edge.yaml"
_URI = "urn:barpay:edge.yaml"


@dataclass(frozen=True)
class Operation:
    id: str
    method: str
    template: str
    pattern: re.Pattern[str]


class Spec:
    def __init__(self, path: Path = SPEC_PATH) -> None:
        self.doc: dict[str, Any] = yaml.safe_load(path.read_text())
        self.prefix: str = self.doc["servers"][0]["url"]
        self._registry: Registry[Any] = Registry().with_resource(
            _URI, Resource.from_contents(self.doc, default_specification=DRAFT202012)
        )
        self.operations: list[Operation] = []
        for template, item in self.doc["paths"].items():
            regex = re.sub(r"\\{(\w+)\\}", r"(?P<\1>[^/]+)", re.escape(template))
            for method, op in item.items():
                if method == "parameters":
                    continue
                self.operations.append(
                    Operation(op["operationId"], method.upper(), template, re.compile(regex))
                )

    def find(self, method: str, path: str) -> tuple[Operation, dict[str, str]] | None:
        """The operation for a request path under the prefix, and its path parameters."""
        for op in self.operations:
            m = op.pattern.fullmatch(path)
            if op.method == method and m:
                return op, m.groupdict()
        return None

    def _op(self, op: Operation) -> dict[str, Any]:
        result: dict[str, Any] = self.doc["paths"][op.template][op.method.lower()]
        return result

    def request_errors(
        self, op: Operation, params: dict[str, str], body: Any, has_body: bool
    ) -> list[str]:
        """Why the request is not one edge.yaml defines, or nothing when it is."""
        errors: list[str] = []
        item = self.doc["paths"][op.template]
        for param in item.get("parameters", []) + self._op(op).get("parameters", []):
            errors += self._errors(param["schema"], params.get(param["name"]))
        request_body = self._op(op).get("requestBody")
        if request_body is None:
            if has_body:
                errors.append("the operation takes no body")
        elif not has_body:
            errors.append("the body is missing")
        else:
            errors += self._errors(request_body["content"]["application/json"]["schema"], body)
        return errors

    def answer_errors(self, op: Operation, status: int, body: bytes) -> list[str]:
        """Why the answer is not one edge.yaml defines for op, or nothing when it is."""
        response = self._op(op)["responses"].get(str(status))
        if response is None:
            return [f"{op.id} defines no {status}"]
        content = response.get("content")
        if content is None:
            return [] if body == b"" else [f"{op.id}'s {status} has no body"]
        try:
            decoded = json.loads(body)
        except ValueError:
            return ["the body is not JSON"]
        return self._errors(content["application/json"]["schema"], decoded)

    def _errors(self, schema: dict[str, Any], instance: Any) -> list[str]:
        if "$ref" in schema:
            schema = {"$ref": _URI + schema["$ref"]}
        validator = Draft202012Validator(schema, registry=self._registry)
        return [e.message for e in validator.iter_errors(instance)]
