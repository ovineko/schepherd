pub const MAX_INPUT_BYTES: usize = 8 * 1024 * 1024;
pub const MAX_DOCUMENTS: usize = 4096;
pub const MAX_DIAGNOSTICS: usize = 1024;
pub const MAX_EXCERPT_BYTES: usize = 1024;
pub const MAX_ALLOCATION_BYTES: usize = 16 * 1024 * 1024;

pub fn excerpt(text: &str) -> &str {
    let mut end = text.len().min(MAX_EXCERPT_BYTES);
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    &text[..end]
}
