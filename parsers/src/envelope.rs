use crate::limits::{excerpt, MAX_DOCUMENTS};
use crate::response::{diagnostic, file, Response};
use crate::validators;
use serde_json::{json, Value};

pub fn parse(bytes: &[u8], process_exit: i32) -> Response {
    let mut response = Response::default();
    let Ok(report) = serde_json::from_slice::<Value>(bytes) else {
        return response;
    };
    if report.get("reportVersion").and_then(Value::as_u64) != Some(2) {
        return response;
    }
    response.recognized = true;
    let Some(tasks) = report.get("tasks").and_then(Value::as_array) else {
        response.incomplete("schepherd", "Malformed Schepherd report: missing tasks");
        return response;
    };
    if report
        .get("exitCode")
        .and_then(Value::as_i64)
        .and_then(|n| i32::try_from(n).ok())
        .is_none()
        || report.get("workspace").and_then(Value::as_str).is_none()
        || !matches!(
            report.get("mode").and_then(Value::as_str),
            Some("batch" | "per-file" | "stdin")
        )
        || report.get("skipped").and_then(Value::as_array).is_none()
    {
        response.incomplete("schepherd", "Malformed Schepherd report metadata");
    }
    if let Some(error) = report.get("error") {
        if let Some(message) = error.get("message").and_then(Value::as_str) {
            let code = error.get("kind").and_then(Value::as_str);
            let path = error.get("file").and_then(Value::as_str);
            let files = string_list(&error["files"]);
            if let Some(path) = path {
                response.push(diagnostic("schepherd", message, Some(path), code, true));
            } else if !files.is_empty() {
                for path in files.iter().take(MAX_DOCUMENTS) {
                    response.push(diagnostic("schepherd", message, Some(path), code, true));
                }
                if files.len() > MAX_DOCUMENTS {
                    response.partial = true;
                }
            } else {
                response.push(diagnostic("schepherd", message, None, code, true));
            }
        } else {
            response.incomplete("schepherd", "Malformed Schepherd run error");
        }
    }
    let workspace = report
        .get("workspace")
        .and_then(Value::as_str)
        .unwrap_or("");
    for task in tasks.iter().take(MAX_DOCUMENTS) {
        parse_task(task, workspace, &mut response);
    }
    if tasks.len() > MAX_DOCUMENTS {
        response.incomplete("schepherd", "Schepherd report exceeds the task limit");
    }
    let failed = report
        .get("exitCode")
        .and_then(Value::as_i64)
        .unwrap_or(process_exit as i64)
        != 0
        || process_exit != 0;
    if failed && response.diagnostics.is_empty() {
        response.incomplete(
            "schepherd",
            "Schepherd failed without a reported diagnostic",
        );
    }
    response
}

fn string_list(value: &Value) -> Vec<&str> {
    value
        .as_array()
        .map(|items| items.iter().filter_map(Value::as_str).collect())
        .unwrap_or_default()
}

struct Stream<'a> {
    text: &'a str,
    incomplete: bool,
}

fn stream(value: &Value) -> Stream<'_> {
    let text = value.get("text").and_then(Value::as_str).unwrap_or("");
    let count = value.get("bytes").and_then(Value::as_u64);
    let truncated = value.get("truncated").and_then(Value::as_bool);
    let incomplete = truncated != Some(false)
        || count.is_none()
        || value.get("base64").is_some()
        || count != Some(text.len() as u64);
    Stream { text, incomplete }
}

fn parse_task(task: &Value, workspace: &str, response: &mut Response) {
    let files = string_list(&task["files"]);
    let paths = string_list(&task["paths"]);
    let cwd = task.get("cwd").and_then(Value::as_str).unwrap_or("");
    if cwd.len() > crate::limits::MAX_EXCERPT_BYTES
        || workspace.len() > crate::limits::MAX_EXCERPT_BYTES
        || files.len().max(paths.len()) > MAX_DOCUMENTS
        || files
            .iter()
            .chain(&paths)
            .any(|path| path.len() > crate::limits::MAX_EXCERPT_BYTES)
        || task
            .get("schemaRef")
            .and_then(Value::as_str)
            .is_some_and(|path| path.len() > crate::limits::MAX_EXCERPT_BYTES)
    {
        response.incomplete(
            "schepherd",
            "Schepherd task attribution exceeds parser limits",
        );
        return;
    }
    let status = task
        .get("status")
        .and_then(Value::as_str)
        .unwrap_or("unknown");
    let exit = task.get("exitCode").and_then(Value::as_i64);
    let output = task["processor"]
        .get("output")
        .and_then(Value::as_str)
        .unwrap_or("");
    let key = match output {
        "sourcemeta-json" => "sourcemeta",
        "check-jsonschema-json" => "check_jsonschema",
        "ajv-json" => "ajv",
        _ => "",
    };
    let stdout = stream(&task["stdout"]);
    let stderr = stream(&task["stderr"]);
    let mut parsed = validators::parse(
        key,
        stdout.text.as_bytes(),
        stderr.text.as_bytes(),
        exit.and_then(|n| i32::try_from(n).ok()).unwrap_or(1),
    );
    let normalized_paths = paths
        .iter()
        .map(|path| normalized(path, cwd))
        .collect::<Vec<_>>();
    let normalized_files = files
        .iter()
        .map(|path| normalized(path, cwd))
        .collect::<Vec<_>>();
    let converted =
        paths.len() == 1 && files.len() == 1 && normalized_paths[0] != normalized_files[0];
    let local_schema = (task.get("origin").and_then(Value::as_str) == Some("local"))
        .then(|| task.get("schemaRef").and_then(Value::as_str))
        .flatten()
        .and_then(|reference| reference.strip_prefix("local:"))
        .filter(|path| !path.is_empty())
        .map(|path| normalized(path, workspace));
    let mut attribution_missing = false;
    for d in &mut parsed.diagnostics {
        let operational = d.get("_operational").and_then(Value::as_bool) == Some(true);
        let named = d.get("file").and_then(Value::as_str).map(str::to_string);
        let normalized_name = named.as_deref().map(|name| normalized(name, cwd));
        let index = normalized_name
            .as_ref()
            .and_then(|name| normalized_paths.iter().position(|path| name == path))
            .or_else(|| {
                normalized_name
                    .as_ref()
                    .and_then(|name| normalized_files.iter().position(|path| name == path))
            });
        if let Some(index) = index.filter(|index| *index < files.len()) {
            if let Some(path) = file(files[index]) {
                d["file"] = json!(path);
            }
            if normalized_paths
                .get(index)
                .is_some_and(|path| path != &normalized_files[index])
            {
                remove_position(d);
            }
        } else if let Some(name) = named {
            if normalized_name
                .as_ref()
                .is_some_and(|path| Some(path) == local_schema.as_ref())
            {
                if let Some(path) = local_schema.as_deref().and_then(file) {
                    d["file"] = json!(path);
                }
            } else {
                d.as_object_mut().map(|o| o.remove("file"));
                remove_position(d);
                let message = d["message"].as_str().unwrap_or("");
                d["message"] = json!(format!(
                    "{} (validator-reported path: {})",
                    excerpt(message),
                    excerpt(&name)
                ));
            }
        } else if files.len() == 1 && !operational {
            if let Some(path) = file(files[0]) {
                d["file"] = json!(path);
            }
            if converted {
                remove_position(d);
            }
        } else if operational {
            remove_position(d);
        } else {
            remove_position(d);
            attribution_missing = true;
        }
    }
    let incomplete =
        stdout.incomplete || stderr.incomplete || parsed.partial || attribution_missing;
    let failed = status != "ok" || exit != Some(0);
    let fallback = incomplete
        || (!parsed.recognized && failed)
        || (failed && parsed.diagnostics.is_empty())
        || !matches!(status, "ok" | "failed");
    response.partial |= incomplete;
    for d in parsed.diagnostics {
        response.push(d);
    }
    if fallback {
        response.partial = true;
        let reason = if stdout.incomplete || stderr.incomplete {
            "incomplete or non-UTF-8 output"
        } else if attribution_missing {
            "anonymous output cannot be attributed to multiple input files"
        } else if !parsed.recognized {
            "unrecognized validator output"
        } else {
            "consumer task did not complete successfully"
        };
        let excerpt_text = if !stderr.text.trim().is_empty() {
            stderr.text
        } else {
            stdout.text
        };
        let message = format!(
            "{}: {} (status {}, exit {}): {}",
            task["processor"]["name"].as_str().unwrap_or("validator"),
            reason,
            status,
            exit.map(|n| n.to_string())
                .unwrap_or_else(|| "unknown".into()),
            excerpt(excerpt_text)
        );
        response.push(diagnostic(
            "schepherd",
            &message,
            if files.len() == 1 {
                file(files[0])
            } else {
                None
            },
            None,
            true,
        ));
    }
}

fn remove_position(d: &mut Value) {
    if let Some(object) = d.as_object_mut() {
        for key in ["row", "col", "end_row", "end_col"] {
            object.remove(key);
        }
    }
}

fn normalized(path: &str, cwd: &str) -> String {
    let windows = path.as_bytes().get(1) == Some(&b':')
        || cwd.as_bytes().get(1) == Some(&b':')
        || path.starts_with("\\\\")
        || cwd.starts_with("\\\\");
    let path = if windows {
        path.replace('\\', "/")
    } else {
        path.to_string()
    };
    let absolute =
        if path.starts_with('/') || path.as_bytes().get(1) == Some(&b':') || cwd.is_empty() {
            path
        } else {
            format!(
                "{}/{}",
                if windows {
                    cwd.replace('\\', "/")
                } else {
                    cwd.to_string()
                }
                .trim_end_matches('/'),
                path
            )
        };
    let rooted = absolute.starts_with('/');
    let mut components = Vec::new();
    for component in absolute.split('/') {
        match component {
            "" | "." => {}
            ".." => {
                if components
                    .last()
                    .is_some_and(|last: &&str| *last != ".." && !last.ends_with(':'))
                {
                    components.pop();
                } else if !rooted && !windows {
                    components.push(component);
                }
            }
            _ => components.push(component),
        }
    }
    let prefix = if absolute.starts_with("//") {
        "//"
    } else if rooted {
        "/"
    } else {
        ""
    };
    format!("{}{}", prefix, components.join("/"))
}
