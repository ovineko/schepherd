use crate::limits::{MAX_DOCUMENTS, MAX_INPUT_BYTES};
use crate::response::{diagnostic, located, position, Response};
use serde_json::Value;

pub fn parse(key: &str, stdout: &[u8], stderr: &[u8], exit_code: i32) -> Response {
    let mut response = Response::default();
    if stdout.len().saturating_add(stderr.len()) > MAX_INPUT_BYTES {
        return response;
    }
    let mut documents = 0;
    let mut malformed = false;
    let streams: &[&[u8]] = if key == "sourcemeta" || key == "check_jsonschema" {
        &[stdout]
    } else {
        &[stdout, stderr]
    };
    for &bytes in streams {
        if bytes.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        let Ok(text) = std::str::from_utf8(bytes) else {
            malformed = true;
            if response.recognized {
                response.incomplete(key, "Validator output contains invalid UTF-8");
            }
            continue;
        };
        let mut input = text.trim();
        let mut named_file = None;
        if key == "ajv" {
            if exit_code == 0
                && input.lines().all(|line| {
                    line.strip_suffix(" valid")
                        .is_some_and(|path| !path.is_empty())
                })
            {
                response.recognized = true;
                continue;
            }
            if let Some((header, rest)) = input.split_once('\n') {
                if let Some(path) = header.strip_suffix(" invalid") {
                    named_file = Some(path);
                    input = rest.trim();
                }
            }
        }
        let mut stream = serde_json::Deserializer::from_str(input).into_iter::<Value>();
        while let Some(next) = stream.next() {
            documents += 1;
            if documents > MAX_DOCUMENTS {
                response.incomplete(key, "Validator output exceeds the JSON document limit");
                break;
            }
            match next {
                Ok(value) => {
                    let start = response.diagnostics.len();
                    let known = match key {
                        "sourcemeta" => sourcemeta(&value, &mut response),
                        "check_jsonschema" => check_jsonschema(&value, &mut response),
                        "ajv" => ajv(&value, &mut response),
                        _ => false,
                    };
                    if known {
                        response.recognized = true;
                        if let Some(path) = named_file.and_then(crate::response::file) {
                            for d in &mut response.diagnostics[start..] {
                                if d.get("file").is_none() {
                                    d["file"] = Value::String(path.to_string());
                                }
                            }
                        }
                    } else if response.recognized {
                        response.incomplete(key, "Unrecognized document in validator output");
                    } else {
                        malformed = true;
                    }
                }
                Err(_) => {
                    malformed = true;
                    if response.recognized {
                        response.incomplete(key, "Incomplete or malformed validator JSON output");
                    }
                    break;
                }
            }
        }
    }
    if malformed && response.recognized && !response.partial {
        response.incomplete(
            key,
            "Validator output contains unrecognized or malformed data",
        );
    }
    if response.recognized && exit_code != 0 && response.diagnostics.is_empty() {
        response.incomplete(key, "Validator failed without a reported diagnostic");
    }
    response
}

fn sourcemeta(value: &Value, response: &mut Response) -> bool {
    if let Some(valid) = value.get("valid").and_then(Value::as_bool) {
        if valid {
            if value
                .get("errors")
                .is_some_and(|errors| errors.as_array().is_none_or(|errors| !errors.is_empty()))
            {
                response.incomplete(
                    "sourcemeta",
                    "Sourcemeta success contains malformed or conflicting errors",
                );
            }
            return true;
        }
        let start = response.diagnostics.len();
        if value.get("errors").is_some_and(|errors| !errors.is_array()) {
            response.incomplete("sourcemeta", "Malformed Sourcemeta error list");
        }
        if let Some(errors) = value.get("errors").and_then(Value::as_array) {
            for error in errors.iter().take(crate::limits::MAX_DIAGNOSTICS + 1) {
                let Some(message) = error.get("error").and_then(Value::as_str) else {
                    response.incomplete("sourcemeta", "Malformed Sourcemeta error entry");
                    continue;
                };
                let code = error
                    .get("keywordLocation")
                    .and_then(Value::as_str)
                    .and_then(|p| p.rsplit('/').next())
                    .filter(|s| !s.is_empty());
                let message = located(
                    message,
                    error.get("instanceLocation").and_then(Value::as_str),
                );
                let mut d = diagnostic("sourcemeta", &message, None, code, true);
                if let Some(pos) = error.get("instancePosition").and_then(Value::as_array) {
                    if pos.len() == 4 {
                        position(&mut d, &pos[0], &pos[1]);
                    }
                }
                response.push(d);
            }
            if errors.len() > crate::limits::MAX_DIAGNOSTICS {
                response.partial = true;
            }
        }
        if response.diagnostics.len() == start {
            response.push(diagnostic(
                "sourcemeta",
                "Schema validation failed without error details",
                None,
                None,
                false,
            ));
        }
        return true;
    }
    if let Some(message) = value.get("error").and_then(Value::as_str) {
        let mut d = diagnostic(
            "sourcemeta",
            message,
            value.get("filePath").and_then(Value::as_str),
            None,
            true,
        );
        d["_operational"] = Value::Bool(true);
        position(&mut d, &value["line"], &value["column"]);
        response.push(d);
        return true;
    }
    false
}

fn check_jsonschema(value: &Value, response: &mut Response) -> bool {
    let Some(status) = value.get("status").and_then(Value::as_str) else {
        return false;
    };
    if status != "ok" && status != "fail" {
        return false;
    }
    let start = response.diagnostics.len();
    for key in ["errors", "parse_errors"] {
        let Some(entries) = value.get(key) else {
            continue;
        };
        let Some(entries) = entries.as_array() else {
            response.incomplete("check_jsonschema", "Malformed check-jsonschema error list");
            continue;
        };
        for entry in entries.iter().take(crate::limits::MAX_DIAGNOSTICS + 1) {
            let Some(message) = entry.get("message").and_then(Value::as_str) else {
                response.incomplete("check_jsonschema", "Malformed check-jsonschema error entry");
                continue;
            };
            let message = located(message, entry.get("path").and_then(Value::as_str));
            response.push(diagnostic(
                "check_jsonschema",
                &message,
                entry.get("filename").and_then(Value::as_str),
                None,
                true,
            ));
        }
        if entries.len() > crate::limits::MAX_DIAGNOSTICS {
            response.partial = true;
        }
    }
    if status == "fail" && response.diagnostics.len() == start {
        response.push(diagnostic(
            "check_jsonschema",
            "Schema validation failed without error details",
            None,
            None,
            false,
        ));
    }
    true
}

fn ajv(value: &Value, response: &mut Response) -> bool {
    if let Some(message) = value.get("error").and_then(Value::as_str) {
        let kind = value.get("kind").and_then(Value::as_str);
        let mut d = diagnostic(
            "ajv",
            message,
            value
                .get("file")
                .or_else(|| value.get("filePath"))
                .and_then(Value::as_str),
            kind,
            true,
        );
        d["_operational"] = Value::Bool(true);
        response.push(d);
        return true;
    }
    let (errors, path) = if let Some(valid) = value.get("valid").and_then(Value::as_bool) {
        if valid {
            if value.get("errors").is_some_and(|errors| {
                !errors.is_null() && errors.as_array().is_none_or(|errors| !errors.is_empty())
            }) {
                response.incomplete(
                    "ajv",
                    "AJV success contains malformed or conflicting errors",
                );
            }
            return true;
        }
        (
            value.get("errors").and_then(Value::as_array),
            value.get("file").and_then(Value::as_str),
        )
    } else if let Some(errors) = value.as_array() {
        if errors.is_empty()
            || !errors
                .iter()
                .any(|e| e.get("keyword").and_then(Value::as_str).is_some())
        {
            return false;
        }
        (Some(errors), None)
    } else {
        return false;
    };
    let Some(errors) = errors else {
        response.incomplete("ajv", "AJV validation failed without an error list");
        return true;
    };
    if errors.is_empty() {
        response.incomplete("ajv", "AJV validation failed with an empty error list");
    }
    for error in errors.iter().take(crate::limits::MAX_DIAGNOSTICS + 1) {
        let (Some(message), Some(keyword), Some(pointer)) = (
            error.get("message").and_then(Value::as_str),
            error.get("keyword").and_then(Value::as_str),
            error.get("instancePath").and_then(Value::as_str),
        ) else {
            response.incomplete("ajv", "Malformed AJV error entry");
            continue;
        };
        let message = located(message, Some(pointer));
        response.push(diagnostic("ajv", &message, path, Some(keyword), true));
    }
    if errors.len() > crate::limits::MAX_DIAGNOSTICS {
        response.partial = true;
    }
    true
}
