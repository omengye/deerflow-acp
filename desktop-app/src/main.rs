// Portable Debug and Release builds both launch as a GUI application. An
// explicit development feature retains the console for `cargo run` and the
// dev watcher without making a packaged Debug build open a terminal.
#![cfg_attr(
    all(
        target_os = "windows",
        any(not(debug_assertions), not(feature = "dev-console"))
    ),
    windows_subsystem = "windows"
)]

fn main() {
    waku::run();
}
