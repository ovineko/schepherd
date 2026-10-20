from __future__ import annotations

import hashlib
import importlib.metadata
import io
import json
import pathlib
import socket
import sys
import tempfile
import unittest
from unittest import mock

import schepherd_check_jsonschema
from schepherd_check_jsonschema import compile_schema, main, validate_file


class FrontendTests(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        network = mock.patch.object(
            socket, "socket", side_effect=AssertionError("network forbidden")
        )
        network.start()
        self.addCleanup(network.stop)

    def write(self, name: str, content: str) -> pathlib.Path:
        path = self.root / name
        path.write_text(content, encoding="utf-8")
        return path

    def invoke(self, *args: object) -> tuple[int, str, str]:
        stdout, stderr = io.StringIO(), io.StringIO()
        code = main([str(arg) for arg in args], stdout=stdout, stderr=stderr)
        return code, stdout.getvalue(), stderr.getvalue()

    def test_identity_reports_exact_runtime_dependencies_and_source_without_schema(
        self,
    ) -> None:
        with mock.patch(
            "schepherd_check_jsonschema.compile_schema",
            side_effect=AssertionError("identity must not access a schema"),
        ):
            code, stdout, stderr = self.invoke("identity")
            self.assertEqual(self.invoke("identity"), (code, stdout, stderr))
        self.assertEqual((code, stderr), (0, ""))
        frontend = pathlib.Path(schepherd_check_jsonschema.__file__)
        expected = {
            "frontend": {"sha256": hashlib.sha256(frontend.read_bytes()).hexdigest()},
            "runtime": {
                "name": "python",
                "version": sys.version,
                "implementation": sys.implementation.name,
            },
            "dependencies": {
                name: importlib.metadata.version(name)
                for name in (
                    "check-jsonschema",
                    "jsonschema",
                    "referencing",
                    "jsonschema-specifications",
                    "regress",
                    "ruamel.yaml",
                    "json5",
                    "attrs",
                    "rpds-py",
                )
            },
        }
        self.assertEqual(json.loads(stdout), expected)
        self.assertNotIn(str(frontend.parent), stdout)

    def test_programming_errors_are_not_misreported_as_invalid_schemas(self) -> None:
        with (
            mock.patch(
                "schepherd_check_jsonschema.compile_schema",
                side_effect=TypeError("implementation defect"),
            ),
            self.assertRaisesRegex(TypeError, "implementation defect"),
        ):
            self.invoke("probe", self.root / "schema.json")

    def test_resource_failures_are_classified_without_a_traceback(self) -> None:
        with mock.patch(
            "schepherd_check_jsonschema.compile_schema",
            side_effect=MemoryError("resource exhausted"),
        ):
            code, stdout, stderr = self.invoke("probe", self.root / "schema.json")
        self.assertEqual((code, stdout), (7, ""))
        self.assertEqual(stderr, "MemoryError: resource exhausted\n")

    def test_probe_checks_schema_and_references_without_instances(self) -> None:
        schema = self.write(
            "schema.json",
            json.dumps(
                {
                    "$schema": "https://json-schema.org/draft/2020-12/schema",
                    "$id": "https://schemas.example/root.json",
                    "$defs": {"word": {"$id": "word.json", "type": "string"}},
                    "properties": {"name": {"$ref": "word.json"}},
                }
            ),
        )
        code, stdout, stderr = self.invoke("probe", schema)
        self.assertEqual(
            (code, json.loads(stdout), stderr), (0, {"supported": True}, "")
        )
        instance = self.write("invalid.json", '{"name": 12}')
        code, stdout, stderr = self.invoke("validate", schema, instance)
        self.assertEqual((code, stderr), (1, ""))
        error = json.loads(stdout)["errors"][0]
        self.assertEqual(error["filename"], str(instance))
        self.assertEqual(error["path"], "$.name")
        self.assertNotIn("line", error)
        self.assertNotIn("column", error)

    def test_json_yaml_toml_json5_jsonc(self) -> None:
        schema = self.write(
            "schema.json",
            '{"type":"object","properties":{"port":{"type":"integer","minimum":10}},"required":["port"]}',
        )
        contents = {
            "document.json": '{"port": 12}',
            "document.yaml": "port: 12\n",
            "document.yml": "port: 12\n",
            "document.toml": "port = 12\n",
            "document.json5": "{port: 12,}",
            "document.jsonc": '{// comment\n"port": 12,}',
        }
        for filename, content in contents.items():
            with self.subTest(filename=filename):
                instance = self.write(filename, content)
                before = instance.read_bytes()
                code, stdout, stderr = self.invoke("validate", schema, instance)
                self.assertEqual((code, stderr), (0, ""))
                self.assertEqual(
                    json.loads(stdout),
                    {"status": "ok", "errors": [], "parse_errors": []},
                )
                self.assertEqual(instance.read_bytes(), before)
        self.assertIn('"required"', schema.read_text())

    def test_multiple_yaml_documents_validate_each_document(self) -> None:
        schema = self.write("schema.json", '{"type":"object","required":["port"]}')
        instance = self.write("stream.yaml", "port: 12\n---\nother: 12\n")
        code, stdout, stderr = self.invoke("validate", schema, instance)
        self.assertEqual((code, stderr), (1, ""))
        errors = json.loads(stdout)["errors"]
        self.assertEqual(len(errors), 1)
        self.assertEqual(errors[0]["filename"], str(instance))
        self.assertIn("port", errors[0]["message"])

    def test_document_parse_failure_is_not_a_schema_failure(self) -> None:
        schema = self.write("schema.json", '{"type":"object"}')
        for name, content in (
            ("broken.json", "{"),
            ("broken.yaml", "["),
            ("unknown.txt", "{}"),
            ("empty.json", "  "),
            ("empty.yaml", "# comment\n"),
        ):
            with self.subTest(name=name):
                instance = self.write(name, content)
                code, stdout, stderr = self.invoke("validate", schema, instance)
                self.assertEqual((code, stderr), (6, ""))
                report = json.loads(stdout)
                self.assertEqual(report["status"], "fail")
                self.assertEqual(report["errors"], [])
                self.assertEqual(report["parse_errors"][0]["filename"], str(instance))

    def test_explicit_yaml_null_is_data(self) -> None:
        schema = self.write("schema.json", '{"type":"null"}')
        for content in ("null\n", "~\n", "---\nnull\n"):
            instance = self.write("document.yaml", content)
            code, stdout, stderr = self.invoke("validate", schema, instance)
            self.assertEqual((code, stderr), (0, ""))
            self.assertEqual(json.loads(stdout)["status"], "ok")

    def test_malformed_schema_is_never_reported_as_unsupported(self) -> None:
        for content in (
            "{",
            "null",
            '"schema"',
            '{"type":12}',
            '{"$schema":12}',
            '{"$schema":"relative"}',
            '{"pattern":"["}',
            '{"$defs":{"bad":{"patternProperties":{"[":{}}}}}',
            '{"$schema":"https://schemas.example/custom","type":12}',
            '{"$defs":{"bad":{"$ref":"#/missing"}}}',
            '{"$vocabulary":{"https://schemas.example/custom":true,"relative":true}}',
            '{"$vocabulary":{"https://schemas.example/custom":true},"$defs":{"bad":{"type":12}}}',
        ):
            with self.subTest(content=content):
                schema = self.write("schema.json", content)
                code, stdout, stderr = self.invoke("probe", schema)
                self.assertEqual(code, 4)
                self.assertEqual(stdout, "")
                self.assertTrue(stderr)

    def test_external_references_never_retrieve_network_or_local_files(self) -> None:
        sibling = self.write("private.json", '{"type":"string"}')
        for reference in (
            "https://schemas.example/unavailable",
            "private.json",
            sibling.as_uri(),
            "#/missing",
        ):
            with self.subTest(reference=reference):
                schema = self.write("schema.json", json.dumps({"$ref": reference}))
                code, stdout, stderr = self.invoke("probe", schema)
                self.assertEqual((code, stdout), (4, ""))
                self.assertIn("external retrieval is disabled", stderr)
        self.assertEqual(sibling.read_text(), '{"type":"string"}')

    def test_external_reference_hidden_in_unvisited_branch_is_checked(self) -> None:
        schema = self.write(
            "schema.json",
            '{"properties":{"never_present":{"$ref":"https://schemas.example/missing"}}}',
        )
        instance = self.write("valid.json", "{}")
        code, stdout, stderr = self.invoke("validate", schema, instance)
        self.assertEqual((code, stdout), (4, ""))
        self.assertIn("unresolved schema reference", stderr)

    def test_unknown_dialect_and_required_vocabulary_are_incompatibility(self) -> None:
        for content in (
            '{"$schema":"https://schemas.example/custom","type":"object"}',
            '{"$vocabulary":{"https://schemas.example/custom":true}}',
            '{"$defs":{"other":{"$schema":"https://schemas.example/custom"}}}',
        ):
            with self.subTest(content=content):
                schema = self.write("schema.json", content)
                code, stdout, stderr = self.invoke("probe", schema)
                self.assertEqual((code, stderr), (0, ""))
                report = json.loads(stdout)
                self.assertFalse(report["supported"])
                self.assertTrue(report["reason"])

    def test_optional_unknown_vocabulary_does_not_disable_validation(self) -> None:
        schema = self.write(
            "schema.json",
            '{"$vocabulary":{"https://schemas.example/custom":false},"type":"integer"}',
        )
        instance = self.write("document.json", '"wrong"')
        code, stdout, stderr = self.invoke("validate", schema, instance)
        self.assertEqual((code, stderr), (1, ""))
        self.assertEqual(json.loads(stdout)["errors"][0]["path"], "$")

    def test_missing_schema_and_input_are_resource_failures(self) -> None:
        missing = self.root / "missing.json"
        code, stdout, stderr = self.invoke("probe", missing)
        self.assertEqual((code, stdout), (7, ""))
        self.assertTrue(stderr)
        schema = self.write("schema.json", "{}")
        code, stdout, stderr = self.invoke("validate", schema, missing)
        self.assertEqual((code, stderr), (7, ""))
        self.assertEqual(
            json.loads(stdout)["parse_errors"][0]["filename"], str(missing)
        )

    def test_api_and_literal_glob_filename(self) -> None:
        schema = self.write("schema.json", '{"type":"integer"}')
        literal = self.write("[*].json", "12")
        self.write("other.json", '"would fail"')
        result, code = validate_file(compile_schema(schema), literal)
        self.assertEqual(code, 0)
        self.assertEqual(result.successes, [str(literal)])

    def test_regex_and_format_use_native_check_jsonschema(self) -> None:
        schema = self.write(
            "schema.json",
            json.dumps(
                {
                    "type": "object",
                    "properties": {
                        "word": {"pattern": r"^\p{Letter}+$"},
                        "date": {"format": "date-time"},
                    },
                }
            ),
        )
        instance = self.write("document.json", '{"word":"abc","date":"not-a-date"}')
        code, stdout, stderr = self.invoke("validate", schema, instance)
        self.assertEqual((code, stderr), (1, ""))
        self.assertEqual(json.loads(stdout)["errors"][0]["path"], "$.date")

    def test_annotations_that_look_like_schemas_are_not_compiled(self) -> None:
        schema = self.write(
            "schema.json",
            '{"default":{"$ref":"https://schemas.example/nope","pattern":"["},"type":"integer"}',
        )
        code, stdout, stderr = self.invoke("probe", schema)
        self.assertEqual(
            (code, json.loads(stdout), stderr), (0, {"supported": True}, "")
        )

    def test_known_dialect_aliases_probe_and_validate_without_rewriting_files(
        self,
    ) -> None:
        for dialect in (
            "https://json-schema.org/draft-07/schema#",
            "https://json-schema.org/draft-07/schema",
            "https://json-schema.org/draft-04/schema#",
            "http://json-schema.org/draft/2020-12/schema#",
        ):
            with self.subTest(dialect=dialect):
                schema = self.write(
                    "schema.json",
                    json.dumps(
                        {
                            "$schema": dialect,
                            "type": "object",
                            "properties": {"port": {"$ref": "#/definitions/port"}},
                            "definitions": {"port": {"type": "integer", "minimum": 10}},
                        }
                    ),
                )
                original = schema.read_bytes()
                code, stdout, stderr = self.invoke("probe", schema)
                self.assertEqual((code, stderr), (0, ""))
                self.assertEqual(json.loads(stdout), {"supported": True})
                for content, expected in (("{port:12}", 0), ("{port:2}", 1)):
                    instance = self.write("document.jsonc", content)
                    code, stdout, stderr = self.invoke("validate", schema, instance)
                    self.assertEqual((code, stderr), (expected, ""))
                    report = json.loads(stdout)
                    self.assertEqual(
                        report["status"], "ok" if expected == 0 else "fail"
                    )
                    if expected:
                        self.assertEqual(report["errors"][0]["path"], "$.port")
                self.assertEqual(schema.read_bytes(), original)

    def test_nested_dialect_alias_preserves_reference_semantics(self) -> None:
        schema = self.write(
            "schema.json",
            json.dumps(
                {
                    "$schema": "https://json-schema.org/draft/2020-12/schema",
                    "$defs": {
                        "port": {
                            "$schema": "https://json-schema.org/draft-07/schema#",
                            "type": "integer",
                        }
                    },
                    "$ref": "#/$defs/port",
                }
            ),
        )
        instance = self.write("document.json", '"wrong"')
        code, stdout, stderr = self.invoke("validate", schema, instance)
        self.assertEqual((code, stderr), (1, ""))
        self.assertEqual(json.loads(stdout)["errors"][0]["path"], "$")

    def test_similar_unknown_dialect_uris_are_not_aliases(self) -> None:
        for dialect in (
            "https://json-schema.org/draft-07/schema#other",
            "https://json-schema.org/draft-07/schema?custom=1",
            "https://json-schema.org/draft-07/schema/",
            "https://schemas.example/draft-07/schema#",
        ):
            schema = self.write("schema.json", json.dumps({"$schema": dialect}))
            code, stdout, stderr = self.invoke("probe", schema)
            self.assertEqual((code, stderr), (0, ""))
            self.assertFalse(json.loads(stdout)["supported"])

    def test_boolean_and_legacy_dialects(self) -> None:
        for schema_value in (
            True,
            {"$schema": "http://json-schema.org/draft-04/schema#", "type": "string"},
            {"$schema": "http://json-schema.org/draft-07/schema#", "type": "string"},
        ):
            with self.subTest(schema=schema_value):
                schema = self.write("schema.json", json.dumps(schema_value))
                code, stdout, stderr = self.invoke("probe", schema)
                self.assertEqual(
                    (code, json.loads(stdout), stderr), (0, {"supported": True}, "")
                )


if __name__ == "__main__":
    unittest.main()
