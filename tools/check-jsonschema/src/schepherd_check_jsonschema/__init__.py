from __future__ import annotations

import argparse
import copy
import hashlib
import importlib.metadata
import io
import json
import math
import pathlib
import sys
from collections.abc import Sequence
from typing import Any, TextIO
from urllib.parse import urlsplit

import jsonschema
from check_jsonschema.formats import FormatOptions, make_format_checker
from check_jsonschema.parsers import ParseError, ParserSet
from check_jsonschema.parsers.yaml import (
    ParseError as YamlParseError,
)
from check_jsonschema.parsers.yaml import (
    _normalize,
    construct_yaml_implementation,
)
from check_jsonschema.regex_variants import RegexImplementation, RegexVariantName
from check_jsonschema.reporter import JsonReporter
from check_jsonschema.result import CheckResult

# These pinned check-jsonschema internals preserve its regex and metaschema semantics.
from check_jsonschema.schema_loader.main import (
    _check_schema,
    _extend_with_pattern_implementation,
)
from jsonschema_specifications import REGISTRY as METASCHEMAS
from referencing import Registry, Resource
from referencing.exceptions import (
    CannotDetermineSpecification,
    NoSuchResource,
    Unresolvable,
)
from referencing.jsonschema import DRAFT202012, specification_with
from regress import RegressError


class UnsupportedSchema(ValueError):
    pass


class FrontendError(ValueError):
    pass


_FILE_FORMATS = {
    ".json": "json",
    ".yaml": "yaml",
    ".yml": "yaml",
    ".toml": "toml",
    ".json5": "json5",
    ".jsonc": "json5",
}
_DIALECT_ALIASES = {
    f"{scheme}://json-schema.org/{draft}/schema{fragment}": canonical
    for draft, canonical in (
        ("draft-03", "http://json-schema.org/draft-03/schema#"),
        ("draft-04", "http://json-schema.org/draft-04/schema#"),
        ("draft-06", "http://json-schema.org/draft-06/schema#"),
        ("draft-07", "http://json-schema.org/draft-07/schema#"),
        ("draft/2019-09", "https://json-schema.org/draft/2019-09/schema"),
        ("draft/2020-12", "https://json-schema.org/draft/2020-12/schema"),
    )
    for scheme in ("http", "https")
    for fragment in ("", "#")
}
_SUPPORTED_VOCABULARIES = frozenset(
    uri
    for draft, names in (
        (
            "2020-12",
            (
                "core",
                "applicator",
                "unevaluated",
                "validation",
                "meta-data",
                "format-annotation",
                "content",
            ),
        ),
        (
            "2019-09",
            ("core", "applicator", "validation", "meta-data", "format", "content"),
        ),
    )
    for uri in (f"https://json-schema.org/draft/{draft}/vocab/{name}" for name in names)
)


def _reject_retrieval(uri: str) -> Resource:
    raise NoSuchResource(ref=uri)


def _require_json_value(value: Any, active: set[int] | None = None) -> None:
    if value is None or isinstance(value, (str, bool, int)):
        return
    if isinstance(value, float):
        if not math.isfinite(value):
            raise FrontendError("non-finite numbers are not JSON data")
        return
    if not isinstance(value, (list, dict)):
        raise FrontendError(f"value of type {type(value).__name__} is not JSON data")
    if active is None:
        active = set()
    identity = id(value)
    if identity in active:
        raise FrontendError("cyclic data is not JSON data")
    active.add(identity)
    try:
        if isinstance(value, dict):
            for key, child in value.items():
                if not isinstance(key, str):
                    raise FrontendError("object keys must be strings")
                _require_json_value(child, active)
        else:
            for child in value:
                _require_json_value(child, active)
    finally:
        active.remove(identity)


def _read_document(path: pathlib.Path, *, schema: bool = False) -> Any:
    file_format = _FILE_FORMATS.get(path.suffix.lower())
    if file_format is None:
        if schema:
            file_format = "json"
        else:
            raise FrontendError(
                f"unsupported document extension: {path.suffix or '(none)'}"
            )
    raw = path.read_bytes()
    if not raw.strip():
        raise FrontendError("document contains no data")
    value = ParserSet().parse_data_with_path(raw, path, "json", file_format)
    if (
        file_format == "yaml"
        and value is None
        and construct_yaml_implementation().compose(io.BytesIO(raw)) is None
    ):
        raise FrontendError("document contains no data")
    _require_json_value(value)
    return value


def _dialect(schema: Any, inherited: type) -> type:
    if not isinstance(schema, (dict, bool)):
        raise FrontendError("a schema must be an object or boolean")
    if not isinstance(schema, dict) or "$schema" not in schema:
        return inherited
    dialect = schema["$schema"]
    if not isinstance(dialect, str) or not urlsplit(dialect).scheme:
        raise FrontendError("$schema must be an absolute dialect URI")
    validator_class = jsonschema.validators.validator_for(schema, default=None)
    if validator_class is None:
        _check_schema(
            jsonschema.Draft202012Validator,
            schema,
            regex_impl=RegexImplementation(RegexVariantName.default),
        )
        raise UnsupportedSchema(f"unsupported schema dialect: {dialect}")
    return validator_class


def _check_vocabularies(schema: dict) -> None:
    vocabularies = schema.get("$vocabulary", {})
    if not isinstance(vocabularies, dict):
        raise FrontendError("$vocabulary must be an object")
    unsupported = []
    for uri, required in vocabularies.items():
        if (
            not isinstance(uri, str)
            or not urlsplit(uri).scheme
            or not isinstance(required, bool)
        ):
            raise FrontendError("$vocabulary must map absolute URIs to booleans")
        if required and uri not in _SUPPORTED_VOCABULARIES:
            unsupported.append(uri)
    if unsupported:
        raise UnsupportedSchema(f"unsupported required vocabulary: {min(unsupported)}")


def _normalize_schema_dialects(schema: Any) -> Any:
    normalized = copy.deepcopy(schema)
    pending = [(normalized, DRAFT202012)]
    while pending:
        contents, specification = pending.pop()
        if not isinstance(contents, dict):
            continue
        dialect = contents.get("$schema")
        if isinstance(dialect, str):
            canonical = _DIALECT_ALIASES.get(dialect)
            if canonical is not None:
                contents["$schema"] = canonical
            specification = specification_with(
                canonical or dialect, default=specification
            )
        pending.extend(
            (child, specification) for child in specification.subresources_of(contents)
        )
    return normalized


def compile_schema(path: pathlib.Path) -> jsonschema.protocols.Validator:
    schema = _normalize_schema_dialects(_read_document(path, schema=True))
    root_class = _dialect(schema, jsonschema.Draft202012Validator)
    regex = RegexImplementation(RegexVariantName.default)
    dialect = (
        root_class.META_SCHEMA["$id"]
        if "$id" in root_class.META_SCHEMA
        else root_class.META_SCHEMA["id"]
    )
    specification = specification_with(dialect, default=DRAFT202012)
    resource = Resource.from_contents(schema, default_specification=specification)
    registry = Registry(retrieve=_reject_retrieval).with_resources(METASCHEMAS.items())
    registry = registry.with_resource(path.resolve().as_uri(), resource).crawl()
    resolver = registry.resolver(path.resolve().as_uri()).in_subresource(resource)
    pending = [(resource, resolver, root_class)]
    seen: set[tuple[int, str]] = set()
    unsupported: list[str] = []
    while pending:
        current, current_resolver, inherited_class = pending.pop()
        contents = current.contents
        # One resource can occur under different bases when a bundle uses relative IDs.
        key = (id(contents), current_resolver._base_uri)
        if key in seen:
            continue
        seen.add(key)
        try:
            validator_class = _dialect(contents, inherited_class)
            _check_schema(validator_class, contents, regex_impl=regex)
        except UnsupportedSchema as error:
            unsupported.append(str(error))
            continue
        if isinstance(contents, dict):
            try:
                _check_vocabularies(contents)
            except UnsupportedSchema as error:
                unsupported.append(str(error))
            patterns = list(contents.get("patternProperties", {}))
            if "pattern" in contents:
                patterns.append(contents["pattern"])
            if any(not regex.check_format(pattern) for pattern in patterns):
                raise FrontendError("schema contains an invalid regular expression")
            for keyword in ("$ref", "$dynamicRef", "$recursiveRef"):
                if keyword not in validator_class.VALIDATORS or keyword not in contents:
                    continue
                reference = contents[keyword]
                if not isinstance(reference, str):
                    raise FrontendError(f"{keyword} must be a string")
                try:
                    resolved = current_resolver.lookup(reference)
                except Unresolvable as error:
                    raise FrontendError(
                        f"unresolved schema reference: {reference}; external retrieval is disabled"
                    ) from error
                target = Resource.from_contents(
                    resolved.contents,
                    default_specification=specification_with(
                        validator_class.META_SCHEMA.get(
                            "$id", validator_class.META_SCHEMA.get("id")
                        ),
                        default=specification,
                    ),
                )
                pending.append((target, resolved.resolver, validator_class))
        for child in current.subresources():
            pending.append(
                (child, current_resolver.in_subresource(child), validator_class)
            )
    if unsupported:
        raise UnsupportedSchema(min(unsupported))
    validator_class = _extend_with_pattern_implementation(root_class, regex)
    return validator_class(
        schema,
        registry=registry,
        _resolver=resolver,
        format_checker=make_format_checker(FormatOptions(regex_impl=regex), dialect),
    )


def _read_instances(path: pathlib.Path) -> list[Any]:
    if _FILE_FORMATS.get(path.suffix.lower()) != "yaml":
        return [_read_document(path)]
    raw = path.read_bytes()
    yaml = construct_yaml_implementation()
    try:
        nodes = list(yaml.compose_all(io.BytesIO(raw)))
        if not nodes or all(node is None for node in nodes):
            raise FrontendError("document contains no data")
        values = [_normalize(value) for value in yaml.load_all(io.BytesIO(raw))]
    except YamlParseError as error:
        raise ParseError(f"Failed to parse {path}: {error}") from error
    for value in values:
        _require_json_value(value)
    return values


def validate_file(
    validator: jsonschema.protocols.Validator, path: pathlib.Path
) -> tuple[CheckResult, int]:
    result = CheckResult()
    try:
        instances = _read_instances(path)
    except (OSError, ValueError, UnicodeError) as error:
        result.record_parse_error(path, ParseError(str(error)))
        return result, 7 if isinstance(error, OSError) else 6
    for instance in instances:
        for error in validator.iter_errors(instance):
            result.record_validation_error(path, error)
    if result.success:
        result.record_validation_success(path)
    return result, 0 if result.success else 1


class _StreamReporter(JsonReporter):
    def __init__(self, stream: TextIO) -> None:
        super().__init__(verbosity=1, pretty=False)
        self.stream = stream

    def _dump(self, data: Any) -> None:
        data.setdefault("parse_errors", [])
        self.stream.write(
            json.dumps(data, ensure_ascii=True, separators=(",", ":")) + "\n"
        )


def main(
    argv: Sequence[str] | None = None,
    *,
    stdout: TextIO | None = None,
    stderr: TextIO | None = None,
) -> int:
    stdout = sys.stdout if stdout is None else stdout
    stderr = sys.stderr if stderr is None else stderr
    parser = argparse.ArgumentParser(prog="schepherd-check-jsonschema")
    commands = parser.add_subparsers(dest="operation", required=True)
    commands.add_parser("identity")
    probe = commands.add_parser("probe")
    probe.add_argument("schema", type=pathlib.Path)
    validate = commands.add_parser("validate")
    validate.add_argument("schema", type=pathlib.Path)
    validate.add_argument("input", type=pathlib.Path)
    arguments = parser.parse_args(argv)
    try:
        if arguments.operation == "identity":
            stdout.write(
                json.dumps(
                    {
                        "frontend": {
                            "sha256": hashlib.sha256(
                                pathlib.Path(__file__).read_bytes()
                            ).hexdigest()
                        },
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
                    },
                    ensure_ascii=True,
                    separators=(",", ":"),
                )
                + "\n"
            )
            return 0
        validator = compile_schema(arguments.schema)
        if arguments.operation == "probe":
            stdout.write('{"supported":true}\n')
            return 0
        result, code = validate_file(validator, arguments.input)
        _StreamReporter(stdout).report_result(result)
        return code
    except UnsupportedSchema as error:
        if arguments.operation == "probe":
            stdout.write(
                json.dumps(
                    {"supported": False, "reason": str(error)}, ensure_ascii=True
                )
                + "\n"
            )
            return 0
        stderr.write(f"unsupported schema: {error}\n")
        return 4
    except (OSError, MemoryError, RecursionError, ImportError) as error:
        stderr.write(f"{type(error).__name__}: {error}\n")
        return 7
    except (
        ValueError,
        YamlParseError,
        jsonschema.exceptions.SchemaError,
        CannotDetermineSpecification,
        Unresolvable,
        RegressError,
    ) as error:
        stderr.write(f"{type(error).__name__}: {error}\n")
        return 4
