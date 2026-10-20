use crate::limits::{excerpt, MAX_DIAGNOSTICS};
use serde_json::{json, Value};

#[derive(Default)]
pub struct Response {
    pub recognized: bool,
    pub partial: bool,
    pub diagnostics: Vec<Value>,
}

impl Response {
    pub fn push(&mut self, mut diagnostic: Value) {
        if let Some(object) = diagnostic.as_object_mut() {
            for key in ["message", "file", "code"] {
                if let Some(value) = object.get_mut(key) {
                    if let Some(text) = value.as_str() {
                        if text.len() > crate::limits::MAX_EXCERPT_BYTES {
                            *value = Value::String(excerpt(text).to_string());
                            self.partial = true;
                        }
                    }
                }
            }
        }
        if self.diagnostics.len() < MAX_DIAGNOSTICS {
            self.diagnostics.push(diagnostic);
        } else {
            self.partial = true;
        }
    }

    pub fn incomplete(&mut self, source: &str, message: &str) {
        self.partial = true;
        self.push(diagnostic(source, message, None, None, false));
    }

    pub fn to_json(mut self, format: &str) -> String {
        for diagnostic in &mut self.diagnostics {
            if let Some(object) = diagnostic.as_object_mut() {
                object.remove("_operational");
            }
        }
        let mut value = json!({"abi":2,"recognized":self.recognized,"format":format,"diagnostics":self.diagnostics});
        if self.partial {
            value["partial"] = json!(true);
        }
        value.to_string()
    }
}

pub fn file(raw: &str) -> Option<&str> {
    match raw.trim() {
        "" | "-" | "stdin" | "<stdin>" => None,
        _ if raw.len() <= crate::limits::MAX_EXCERPT_BYTES => Some(raw),
        _ => None,
    }
}

pub fn diagnostic(
    source: &str,
    message: &str,
    path: Option<&str>,
    code: Option<&str>,
    error: bool,
) -> Value {
    let mut value = json!({"message":excerpt(message),"source":source});
    if let Some(path) = path.and_then(file) {
        value["file"] = json!(path);
    }
    if let Some(code) = code.filter(|s| !s.is_empty() && s.len() <= 128) {
        value["code"] = json!(code);
    }
    if error {
        value["severity"] = json!(1);
    }
    value
}

pub fn positive(value: &Value) -> Option<u32> {
    value
        .as_u64()
        .and_then(|n| u32::try_from(n).ok())
        .filter(|n| *n > 0)
}

pub fn position(diagnostic: &mut Value, row: &Value, col: &Value) {
    if let Some(row) = positive(row) {
        diagnostic["row"] = json!(row);
        if let Some(col) = positive(col) {
            diagnostic["col"] = json!(col);
        }
    }
}

pub fn located(message: &str, path: Option<&str>) -> String {
    match path {
        Some("") => format!("instance pointer \"\": {}", excerpt(message)),
        Some(path) => format!("instance {}: {}", excerpt(path), excerpt(message)),
        None => excerpt(message).to_string(),
    }
}
