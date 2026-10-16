//! Validate/round-trip a portable project without opening a window or networking.
use anyhow::{Context, Result};
use maildesk_core::{Workspace, atomic_json};
fn main() -> Result<()> {
    let mut args = std::env::args_os().skip(1);
    let input = args
        .next()
        .context("Usage: project_check INPUT.json [OUTPUT.json]")?;
    let project: Workspace = serde_json::from_reader(std::fs::File::open(input)?)?;
    project.validate()?;
    if let Some(output) = args.next() {
        atomic_json(std::path::Path::new(&output), &project)?;
    }
    println!(
        "Valid project v{}: {} messages, {} files",
        project.version,
        project.leads.len(),
        project.files.len()
    );
    Ok(())
}
