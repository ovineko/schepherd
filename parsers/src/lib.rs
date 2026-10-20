use serde_json::json;
use std::alloc::{alloc as allocate, dealloc as free, Layout};
use std::ptr;

mod envelope;
mod limits;
mod response;
mod validators;

pub fn dispatch(key: &str, stdout: &[u8], stderr: &[u8], exit_code: i32) -> String {
    if !matches!(key, "schepherd" | "sourcemeta" | "check_jsonschema" | "ajv") {
        return response::Response::default().to_json("");
    }
    if stdout.len().saturating_add(stderr.len()) > limits::MAX_INPUT_BYTES {
        return response::Response::default().to_json(key);
    }
    let result = if key == "schepherd" {
        envelope::parse(stdout, exit_code)
    } else {
        validators::parse(key, stdout, stderr, exit_code)
    };
    result.to_json(key)
}

pub fn describe_json() -> String {
    let version = option_env!("SCHEPHERD_PARSERS_VERSION").unwrap_or(env!("CARGO_PKG_VERSION"));
    let tools = [
        (
            "schepherd",
            "Schepherd attributed validation report",
            "https://github.com/ovineko/schepherd",
            json!({"lint":{"args":["run","--json","--","{files...}"],"stdin":false}}),
        ),
        (
            "sourcemeta",
            "Sourcemeta JSON Schema validation output",
            "https://github.com/sourcemeta/jsonschema",
            json!({}),
        ),
        (
            "check_jsonschema",
            "check-jsonschema JSON validation output",
            "https://github.com/python-jsonschema/check-jsonschema",
            json!({}),
        ),
        (
            "ajv",
            "AJV JSON validation errors",
            "https://github.com/ajv-validator/ajv-cli",
            json!({}),
        ),
    ]
    .into_iter()
    .map(|(name, description, url, operations)| {
        json!({
            "name":name,"description":description,"url":url,"operations":operations,
            "severities":["error","errors","parse_errors"],"columnUnit":"utf-8","kind":"tool"
        })
    })
    .collect::<Vec<_>>();
    json!({"schemaVersion":3,"module":"schepherd-parsers","version":version,"abi":2,"features":["tools"],"tools":tools}).to_string()
}

#[no_mangle]
pub extern "C" fn alloc(len: u32) -> *mut u8 {
    if len == 0 || len as usize > limits::MAX_ALLOCATION_BYTES {
        return ptr::null_mut();
    }
    let Ok(layout) = Layout::from_size_align(len as usize, 1) else {
        return ptr::null_mut();
    };
    unsafe { allocate(layout) }
}

/// # Safety
/// The pointer and length must match a live allocation returned by this module.
#[no_mangle]
pub unsafe extern "C" fn dealloc(ptr: *mut u8, len: u32) {
    if !ptr.is_null() && len != 0 && len as usize <= limits::MAX_ALLOCATION_BYTES {
        if let Ok(layout) = Layout::from_size_align(len as usize, 1) {
            free(ptr, layout);
        }
    }
}

/// # Safety
/// Each pointer/length pair must name a live allocation returned by `alloc`.
#[no_mangle]
pub unsafe extern "C" fn parse(
    tool_ptr: *const u8,
    tool_len: u32,
    stdout_ptr: *const u8,
    stdout_len: u32,
    stderr_ptr: *const u8,
    stderr_len: u32,
    exit_code: i32,
) -> u64 {
    if tool_len > 128
        || stdout_len as usize > limits::MAX_INPUT_BYTES
        || stderr_len as usize > limits::MAX_INPUT_BYTES
        || (tool_len != 0 && tool_ptr.is_null())
        || (stdout_len != 0 && stdout_ptr.is_null())
        || (stderr_len != 0 && stderr_ptr.is_null())
    {
        return pack(response::Response::default().to_json(""));
    }
    let tool = input(tool_ptr, tool_len);
    let stdout = input(stdout_ptr, stdout_len);
    let stderr = input(stderr_ptr, stderr_len);
    pack(dispatch(
        std::str::from_utf8(tool).unwrap_or(""),
        stdout,
        stderr,
        exit_code,
    ))
}

#[no_mangle]
pub extern "C" fn describe() -> u64 {
    pack(describe_json())
}

#[no_mangle]
pub extern "C" fn reset() {}

unsafe fn input<'a>(pointer: *const u8, len: u32) -> &'a [u8] {
    if len == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(pointer, len as usize)
    }
}

fn pack(json: String) -> u64 {
    let bytes = json.as_bytes();
    let Ok(len) = u32::try_from(bytes.len()) else {
        return 0;
    };
    let pointer = alloc(len);
    if pointer.is_null() {
        return 0;
    }
    unsafe {
        ptr::copy_nonoverlapping(bytes.as_ptr(), pointer, bytes.len());
    }
    ((pointer as u64) << 32) | u64::from(len)
}
