use crate::settings::Settings;
use std::net::TcpStream;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;
use tauri::Manager;
use tauri_plugin_shell::process::CommandChild;
use tauri_plugin_shell::ShellExt;

pub struct SidecarManager {
    child: Arc<Mutex<Option<CommandChild>>>,
}

impl SidecarManager {
    pub fn new() -> Self {
        Self {
            child: Arc::new(Mutex::new(None)),
        }
    }

    pub fn spawn(&self, app: &tauri::AppHandle, settings: &Settings) {
        let args = settings.to_sidecar_args();
        eprintln!("[desktop] spawning sidecar with args: {:?}", args);
        let sidecar = app
            .shell()
            .sidecar("copilot-token-cost")
            .expect("failed to create sidecar command")
            .args(&args);

        let (_rx, child) = sidecar.spawn().expect("failed to spawn sidecar");
        *self.child.lock().unwrap() = Some(child);
    }

    pub fn restart(&self, app: &tauri::AppHandle, settings: &Settings) {
        self.stop();

        let app = app.clone();
        let settings = settings.clone();
        let child = self.child.clone();

        thread::spawn(move || {
            // Wait for port to be released
            thread::sleep(Duration::from_millis(500));

            let args = settings.to_sidecar_args();
            eprintln!("[desktop] restarting sidecar with args: {:?}", args);

            let sidecar = app
                .shell()
                .sidecar("copilot-token-cost")
                .expect("failed to create sidecar command")
                .args(&args);

            match sidecar.spawn() {
                Ok((_rx, new_child)) => {
                    *child.lock().unwrap() = Some(new_child);

                    // Wait for server to be ready
                    for _ in 0..30 {
                        thread::sleep(Duration::from_millis(300));
                        if TcpStream::connect("127.0.0.1:7332").is_ok() {
                            eprintln!("[desktop] sidecar ready");
                            break;
                        }
                    }

                    if let Some(w) = app.get_webview_window("main") {
                        let _ = w.eval(
                            "window.location.replace('http://127.0.0.1:7332')",
                        );
                        let _ = w.show();
                        let _ = w.set_focus();
                    }
                }
                Err(e) => eprintln!("[desktop] failed to spawn sidecar: {}", e),
            }
        });
    }

    pub fn stop(&self) {
        if let Some(child) = self.child.lock().unwrap().take() {
            let _ = child.kill();
        }
    }
}
