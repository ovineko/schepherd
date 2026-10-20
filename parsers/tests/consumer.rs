use schepherd_parsers::{describe_json, dispatch};
use serde_json::{json, Value};

fn parse(key: &str, stdout: &str, code: i32) -> Value {
    serde_json::from_str(&dispatch(key, stdout.as_bytes(), b"", code)).unwrap()
}

fn task(output: &str, text: &str, code: i32) -> Value {
    json!({
        "schemaId":"example", "schemaRef":"/cache/schema.json", "origin":"catalog",
        "files":["/work/input.yaml"], "paths":["/work/input.yaml"], "cwd":"/work",
        "processor":{"name":"validator","type":"sourcemeta","output":output,"mode":"per-file"},
        "status":if code == 0 {"ok"} else {"failed"}, "exitCode":code,"durationMs":1,
        "stdout":{"bytes":text.len(),"text":text,"truncated":false},
        "stderr":{"bytes":0,"text":"","truncated":false}
    })
}

fn report(tasks: Vec<Value>, code: i32) -> Value {
    json!({"reportVersion":2,"workspace":"/work","mode":"per-file","exitCode":code,"tasks":tasks,"skipped":[]})
}

#[test]
fn sourcemeta_real_basic_output_keeps_pointer_keyword_and_start_only() {
    let out = parse(
        "sourcemeta",
        r#"{"valid":false,"errors":[{"keywordLocation":"/properties/foo/type","absoluteKeywordLocation":"file:///cache/schema.json#/properties/foo/type","instanceLocation":"/foo","instancePosition":[1,3,1,10],"error":"The value was expected to be of type string but it was of type integer"}]}"#,
        2,
    );
    let d = &out["diagnostics"][0];
    assert_eq!(out["abi"], 2);
    assert_eq!(d["code"], "type");
    assert!(d["message"].as_str().unwrap().contains("/foo"));
    assert_eq!(d["row"], 1);
    assert_eq!(d["col"], 3);
    assert!(d.get("end_col").is_none());
    assert!(d.get("file").is_none());
}

#[test]
fn annotations_and_flag_success_are_not_errors() {
    let out = parse(
        "sourcemeta",
        r#"{"valid":true,"annotations":[{"instanceLocation":"","annotation":"A title"}]} {"valid":true}"#,
        0,
    );
    assert_eq!(out["recognized"], true);
    assert_eq!(out["diagnostics"], json!([]));
}

#[test]
fn yaml_document_outputs_are_not_assigned_to_different_files() {
    let text = r#"{"valid":true} {"valid":false,"errors":[{"keywordLocation":"/type","instanceLocation":"","instancePosition":[4,1,5,0],"error":"Expected object"}]}"#;
    let out = parse(
        "schepherd",
        &report(vec![task("sourcemeta-json", text, 2)], 2).to_string(),
        2,
    );
    assert_eq!(out["diagnostics"][0]["file"], "/work/input.yaml");
    assert_eq!(out["diagnostics"][0]["row"], 4);
}

#[test]
fn anonymous_multi_file_tasks_never_guess_by_document_index() {
    let mut t = task(
        "sourcemeta-json",
        r#"{"valid":false,"errors":[{"keywordLocation":"/type","instanceLocation":"","error":"Expected object"}]}"#,
        2,
    );
    t["files"] = json!(["/work/a.yaml", "/work/b.yaml"]);
    t["paths"] = t["files"].clone();
    let out = parse("schepherd", &report(vec![t], 2).to_string(), 2);
    for d in out["diagnostics"].as_array().unwrap() {
        assert!(d.get("file").is_none());
    }
    assert_eq!(out["partial"], true);
}

#[test]
fn check_jsonschema_real_errors_and_parse_errors_are_preserved() {
    let out = parse(
        "check_jsonschema",
        r#"{"status":"fail","errors":[{"filename":"C:\\work\\é.yaml","path":"$.name","message":"3 is not of type 'string'","has_sub_errors":false}],"parse_errors":[{"filename":"broken.yaml","message":"Failed to parse YAML"}]}"#,
        1,
    );
    assert_eq!(out["diagnostics"].as_array().unwrap().len(), 2);
    assert_eq!(out["diagnostics"][0]["file"], "C:\\work\\é.yaml");
    assert!(out["diagnostics"][0]["message"]
        .as_str()
        .unwrap()
        .contains("$.name"));
    assert!(out["diagnostics"][1].get("row").is_none());
    assert_eq!(
        parse("check_jsonschema", r#"{"status":"ok","errors":[]}"#, 0)["diagnostics"],
        json!([])
    );
}

#[test]
fn ajv_native_errors_and_adapter_output_keep_rfc6901_pointer() {
    let errors = r##"[{"instancePath":"/a~1b/~0name","schemaPath":"#/properties/a~1b/type","keyword":"type","params":{"type":"string"},"message":"must be string"}]"##;
    let out = parse("ajv", errors, 1);
    assert_eq!(out["diagnostics"][0]["code"], "type");
    assert!(out["diagnostics"][0]["message"]
        .as_str()
        .unwrap()
        .contains("/a~1b/~0name"));
    let out = parse(
        "ajv",
        &format!(r#"{{"valid":false,"errors":{errors},"file":"a.json"}}"#),
        1,
    );
    assert_eq!(out["diagnostics"][0]["file"], "a.json");
    assert_eq!(
        parse("ajv", r#"{"valid":true,"errors":null}"#, 0)["diagnostics"],
        json!([])
    );
}

#[test]
fn converted_inputs_map_files_but_never_source_coordinates() {
    let mut t = task(
        "sourcemeta-json",
        r#"{"error":"Failed to parse the JSON document","filePath":"/tmp/converted.json","line":2,"column":9}"#,
        6,
    );
    t["paths"] = json!(["/tmp/converted.json"]);
    let out = parse("schepherd", &report(vec![t], 6).to_string(), 6);
    assert_eq!(out["diagnostics"][0]["file"], "/work/input.yaml");
    assert!(out["diagnostics"][0].get("row").is_none());
    assert!(out["diagnostics"][0].get("col").is_none());
}

#[test]
fn schema_error_never_becomes_an_instance_location() {
    for (origin, schema_ref, expected_file) in [
        ("catalog", "/cache/schema.json", None),
        ("local", "local:schema.json", Some("/work/schema.json")),
    ] {
        let text = r#"{"error":"Failed to parse the JSON document","filePath":"/work/schema.json","line":2,"column":10}"#;
        let mut t = task("sourcemeta-json", text, 6);
        t["origin"] = json!(origin);
        t["schemaRef"] = json!(schema_ref);
        t["files"] = json!(["/work/input.json"]);
        t["paths"] = t["files"].clone();
        let out = parse("schepherd", &report(vec![t], 6).to_string(), 6);
        let d = &out["diagnostics"][0];
        if let Some(path) = expected_file {
            assert_eq!(d["file"], path);
            assert_eq!(d["row"], 2);
            assert_eq!(d["col"], 10);
        } else {
            assert!(d.get("file").is_none());
            assert!(d.get("row").is_none());
            assert!(d.get("col").is_none());
        }
    }
}

#[test]
fn local_schema_paths_resolve_against_workspace_not_task_cwd() {
    for (workspace, cwd, schema_ref, validator_path, expected_file) in [
        (
            "/work",
            "/work/nested",
            "local:schema.json",
            "../schema.json",
            "/work/schema.json",
        ),
        (
            "/work",
            "/work/nested",
            "local:schema.json",
            "/work/schema.json",
            "/work/schema.json",
        ),
        (
            "/work",
            "/work/nested",
            "local:/external/schema.json",
            "../../external/schema.json",
            "/external/schema.json",
        ),
        (
            "/work",
            "/work/nested",
            "local:/external/schema.json",
            "/external/schema.json",
            "/external/schema.json",
        ),
        (
            r"C:\work",
            r"C:\work\nested",
            r"local:schemas\schema.json",
            r"..\schemas\schema.json",
            "C:/work/schemas/schema.json",
        ),
        (
            r"C:\work",
            r"C:\work\nested",
            r"local:D:\external\schema.json",
            "D:/external/schema.json",
            "D:/external/schema.json",
        ),
        (
            r"C:\work",
            r"C:\work\nested",
            "local:D:/external/schema.json",
            r"D:\external\schema.json",
            "D:/external/schema.json",
        ),
        (
            r"\\server\share\project",
            r"\\server\share\project\nested",
            "local:schema.json",
            r"\\server\share\project\schema.json",
            "//server/share/project/schema.json",
        ),
        (
            r"\\server\share\project",
            r"\\server\share\project\nested",
            "local:schema.json",
            r"..\schema.json",
            "//server/share/project/schema.json",
        ),
    ] {
        let text = json!({"error":"Failed to parse the JSON document","filePath":validator_path,"line":2,"column":10}).to_string();
        let mut t = task("sourcemeta-json", &text, 6);
        t["origin"] = json!("local");
        t["schemaRef"] = json!(schema_ref);
        t["cwd"] = json!(cwd);
        t["paths"] = json!(["/tmp/converted.json"]);
        let mut r = report(vec![t], 6);
        r["workspace"] = json!(workspace);
        let out = parse("schepherd", &r.to_string(), 6);
        let d = &out["diagnostics"][0];
        assert_eq!(d["file"], expected_file, "{schema_ref}: {validator_path}");
        assert_eq!(d["row"], 2);
        assert_eq!(d["col"], 10);
        assert_ne!(out["partial"], true);
    }
}

#[test]
fn unrelated_operational_paths_are_not_assigned_to_local_schema_or_input() {
    for validator_path in ["schema.json", "/external/schema.json", "local:schema.json"] {
        let text = json!({"error":"Failed to parse the JSON document","filePath":validator_path,"line":2,"column":10}).to_string();
        let mut t = task("sourcemeta-json", &text, 6);
        t["origin"] = json!("local");
        t["schemaRef"] = json!("local:schema.json");
        t["cwd"] = json!("/work/nested");
        let out = parse("schepherd", &report(vec![t], 6).to_string(), 6);
        let d = &out["diagnostics"][0];
        assert!(d.get("file").is_none(), "{validator_path}");
        assert!(d.get("row").is_none());
        assert!(d.get("col").is_none());
        assert!(d["message"].as_str().unwrap().contains(validator_path));
    }
}

#[test]
fn truncated_or_unknown_failures_are_visible_with_real_task_file() {
    for truncated in [true, false] {
        let mut t = task("sourcemeta-json", "not JSON: crashed", 1);
        t["stdout"]["truncated"] = json!(truncated);
        let out = parse("schepherd", &report(vec![t], 1).to_string(), 1);
        assert_eq!(out["diagnostics"][0]["file"], "/work/input.yaml");
        assert_eq!(out["partial"], true);
        assert!(out["diagnostics"][0]["message"]
            .as_str()
            .unwrap()
            .contains("crashed"));
    }
}

#[test]
fn startup_timeout_and_base64_failures_cannot_look_successful() {
    for status in ["start-error", "timeout", "canceled", "not-started"] {
        let mut t = task("sourcemeta-json", "", 0);
        t["status"] = json!(status);
        t["stderr"] = json!({"bytes":2,"base64":"//4=","truncated":false});
        let out = parse("schepherd", &report(vec![t], 1).to_string(), 1);
        assert!(!out["diagnostics"].as_array().unwrap().is_empty());
    }
}

#[test]
fn early_errors_keep_classification_and_attribution() {
    let mut r = report(vec![], 2);
    r["error"] =
        json!({"kind":"config","message":"Invalid processor","file":"/work/schepherd.toml"});
    let out = parse("schepherd", &r.to_string(), 2);
    assert_eq!(out["diagnostics"][0]["file"], "/work/schepherd.toml");
    assert_eq!(out["diagnostics"][0]["code"], "config");
}

#[test]
fn unrelated_json_is_not_recognized() {
    for key in [
        "sourcemeta",
        "check_jsonschema",
        "ajv",
        "schepherd",
        "unknown",
    ] {
        assert_eq!(
            parse(key, r#"{"message":"not a validator result"}"#, 1)["recognized"],
            false
        );
    }
}

#[test]
fn malformed_suffix_preserves_previous_findings_and_marks_partial() {
    let out = parse(
        "sourcemeta",
        r#"{"valid":false,"errors":[{"error":"Bad type","keywordLocation":"/type","instanceLocation":""}]} {"valid":"#,
        2,
    );
    assert_eq!(out["recognized"], true);
    assert_eq!(out["partial"], true);
    assert!(!out["diagnostics"].as_array().unwrap().is_empty());
}

#[test]
fn deep_or_oversized_input_cannot_overflow_parser_stack() {
    let deep = format!("{}0{}", "[".repeat(10000), "]".repeat(10000));
    assert_eq!(parse("ajv", &deep, 1)["recognized"], false);
    let large = " ".repeat(9 * 1024 * 1024);
    assert_eq!(parse("schepherd", &large, 1)["recognized"], false);
}

#[test]
fn malformed_known_result_does_not_become_clean_success() {
    let out = parse("sourcemeta", r#"{"valid":false,"errors":[{"error":3}]}"#, 2);
    assert_eq!(out["partial"], true);
    assert!(!out["diagnostics"].as_array().unwrap().is_empty());
}

#[test]
fn descriptor_advertises_exact_dispatch_and_column_contract() {
    let desc: Value = serde_json::from_str(&describe_json()).unwrap();
    assert_eq!(desc["schemaVersion"], 3);
    assert_eq!(desc["abi"], 2);
    for name in ["schepherd", "sourcemeta", "check_jsonschema", "ajv"] {
        let d = desc["tools"]
            .as_array()
            .unwrap()
            .iter()
            .find(|d| d["name"] == name)
            .unwrap();
        assert_eq!(d["kind"], "tool");
        assert_eq!(d["columnUnit"], "utf-8");
    }
}

#[test]
fn utf8_byte_columns_are_preserved_without_character_conversion() {
    let out = parse(
        "sourcemeta",
        r#"{"valid":false,"errors":[{"keywordLocation":"/type","instanceLocation":"/é","instancePosition":[1,12,1,15],"error":"Wrong type"}]}"#,
        2,
    );
    assert_eq!(out["diagnostics"][0]["col"], 12);
    assert!(out["diagnostics"][0]["message"]
        .as_str()
        .unwrap()
        .contains("/é"));
}

#[test]
fn cli_ajv_stderr_wrapper_is_not_lost() {
    let out: Value = serde_json::from_str(&dispatch("ajv", b"", br##"C:\work\input.json invalid
[{"instancePath":"/name","schemaPath":"#/type","keyword":"type","params":{},"message":"must be string"}]"##, 1)).unwrap();
    assert_eq!(out["recognized"], true);
    assert_eq!(out["diagnostics"][0]["file"], "C:\\work\\input.json");
    assert_eq!(
        parse("ajv", "input.json valid\n", 0)["diagnostics"],
        json!([])
    );
}

#[test]
fn pathless_schema_operational_error_is_not_assigned_to_instance() {
    let text = r#"{"error":"Cannot resolve schema reference","identifier":"urn:missing"}"#;
    for origin in ["catalog", "local"] {
        let mut t = task("sourcemeta-json", text, 1);
        t["origin"] = json!(origin);
        t["schemaRef"] = json!("local:schema.json");
        let out = parse("schepherd", &report(vec![t], 1).to_string(), 1);
        assert!(out["diagnostics"][0].get("file").is_none());
        assert!(out["diagnostics"][0].get("row").is_none());
        assert!(out["diagnostics"][0].get("_operational").is_none());
    }
}

#[test]
fn windows_relative_instance_names_map_to_original_input() {
    let mut t = task(
        "check-jsonschema-json",
        r#"{"status":"fail","errors":[{"filename":"input.yaml","path":"$","message":"bad value"}],"parse_errors":[]}"#,
        1,
    );
    t["cwd"] = json!("C:\\work");
    t["files"] = json!(["C:\\work\\input.yaml"]);
    t["paths"] = t["files"].clone();
    let out = parse("schepherd", &report(vec![t], 1).to_string(), 1);
    assert_eq!(out["diagnostics"][0]["file"], "C:\\work\\input.yaml");
}

#[test]
fn exceeding_diagnostic_limit_is_explicitly_partial() {
    let errors = (0..1100)
        .map(|_| json!({"error":"bad type","keywordLocation":"/type","instanceLocation":""}))
        .collect::<Vec<_>>();
    let text = json!({"valid":false,"errors":errors}).to_string();
    let out = parse("sourcemeta", &text, 2);
    assert_eq!(out["partial"], true);
    assert!(out["diagnostics"].as_array().unwrap().len() <= 1024);
}

#[test]
fn malformed_stream_and_task_metadata_fail_visibly_without_panics() {
    let mut t = task("sourcemeta-json", r#"{"valid":true}"#, 0);
    t["stdout"]["bytes"] = json!(999);
    let out = parse("schepherd", &report(vec![t, json!(null)], 1).to_string(), 1);
    assert_eq!(out["partial"], true);
    assert!(!out["diagnostics"].as_array().unwrap().is_empty());
    assert_eq!(
        parse("schepherd", r#"{"reportVersion":2,"tasks":null}"#, 1)["partial"],
        true
    );
}

#[test]
fn ajv_adapter_schema_error_retains_kind_without_instance_attribution() {
    let out = parse(
        "schepherd",
        &report(
            vec![task(
                "ajv-json",
                r#"{"kind":"schema","error":"Schema is invalid","file":"/cache/schema.json"}"#,
                4,
            )],
            4,
        )
        .to_string(),
        4,
    );
    assert_eq!(out["diagnostics"][0]["code"], "schema");
    assert_eq!(out["diagnostics"][0]["source"], "ajv");
    assert!(out["diagnostics"][0].get("file").is_none());
}

#[test]
fn numeric_coordinates_do_not_wrap_or_create_end_ranges() {
    let out = parse(
        "sourcemeta",
        r#"{"error":"Bad document","line":4294967296,"column":3,"filePath":"input.json"}"#,
        6,
    );
    assert!(out["diagnostics"][0].get("row").is_none());
    assert!(out["diagnostics"][0].get("col").is_none());
    let out = parse(
        "sourcemeta",
        r#"{"error":"Bad document","line":2,"column":0,"filePath":"input.json"}"#,
        6,
    );
    assert_eq!(out["diagnostics"][0]["row"], 2);
    assert!(out["diagnostics"][0].get("col").is_none());
    assert!(out["diagnostics"][0].get("end_row").is_none());
}

#[test]
fn reusable_module_reset_carries_no_prior_findings() {
    let first = parse(
        "ajv",
        r#"{"valid":false,"errors":[{"instancePath":"","keyword":"type","message":"must be object"}]}"#,
        1,
    );
    assert_eq!(first["diagnostics"].as_array().unwrap().len(), 1);
    schepherd_parsers::reset();
    let next = parse("ajv", r#"{"valid":true,"errors":[]}"#, 0);
    assert_eq!(next["diagnostics"], json!([]));
}

#[test]
fn explicit_failures_and_conflicting_success_are_never_clean() {
    for (key, text) in [
        ("sourcemeta", r#"{"valid":false}"#),
        (
            "sourcemeta",
            r#"{"valid":true,"errors":[{"error":"conflict"}]}"#,
        ),
        ("check_jsonschema", r#"{"status":"fail"}"#),
        ("ajv", r#"{"valid":false,"errors":[]}"#),
        ("ajv", r#"{"valid":true,"errors":{}}"#),
    ] {
        let out = parse(key, text, 1);
        assert_eq!(out["recognized"], true);
        assert!(
            !out["diagnostics"].as_array().unwrap().is_empty(),
            "{key}: {text}"
        );
    }
}

#[test]
fn invalid_utf8_next_to_known_output_is_partial() {
    let out: Value =
        serde_json::from_str(&dispatch("ajv", br#"{"valid":true}"#, &[0xff], 1)).unwrap();
    assert_eq!(out["recognized"], true);
    assert_eq!(out["partial"], true);
    assert!(!out["diagnostics"].as_array().unwrap().is_empty());
}

#[test]
fn excessive_json_documents_are_not_silently_discarded() {
    let out = parse("sourcemeta", &"{\"valid\":true}\n".repeat(4100), 0);
    assert_eq!(out["recognized"], true);
    assert_eq!(out["partial"], true);
    assert!(!out["diagnostics"].as_array().unwrap().is_empty());
}

#[test]
fn sourcemeta_filename_stderr_is_not_a_malformed_json_document() {
    for (text, exit, expected) in [
        ("{\n  \"valid\": true\n}\n", 0, 0),
        (
            r#"{"valid":false,"errors":[{"error":"Wrong type","keywordLocation":"/type","instanceLocation":""}]}"#,
            2,
            1,
        ),
    ] {
        let stderr = ".github/zizmor.yml\n";
        let out: Value = serde_json::from_str(&dispatch(
            "sourcemeta",
            text.as_bytes(),
            stderr.as_bytes(),
            exit,
        ))
        .unwrap();
        assert_eq!(out["recognized"], true);
        assert!(out.get("partial").is_none());
        assert_eq!(out["diagnostics"].as_array().unwrap().len(), expected);
        let mut t = task("sourcemeta-json", text, exit);
        t["stderr"] = json!({"bytes":stderr.len(),"text":stderr,"truncated":false});
        let out = parse("schepherd", &report(vec![t], exit).to_string(), exit);
        assert!(out.get("partial").is_none());
        assert_eq!(out["diagnostics"].as_array().unwrap().len(), expected);
    }
}

#[test]
fn failed_unknown_stderr_still_reports_explicit_error_severity() {
    let mut t = task("sourcemeta-json", "", 6);
    let stderr = "Validator crashed";
    t["stderr"] = json!({"bytes":stderr.len(),"text":stderr,"truncated":false});
    let out = parse("schepherd", &report(vec![t], 6).to_string(), 6);
    assert_eq!(out["partial"], true);
    assert_eq!(out["diagnostics"][0]["severity"], 1);
    assert!(out["diagnostics"][0]["message"]
        .as_str()
        .unwrap()
        .contains(stderr));
}
