use crate::settings::Settings;
use std::sync::{Arc, Mutex};
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
        self.spawn(app, settings);

        // Navigate webview back to loading page so it polls /healthz
        if let Some(w) = app.get_webview_window("main") {
            let _ = w
                .navigate("tauri://localhost/index.html".parse().unwrap());
            let _ = w.show();
            let _ = w.set_focus();
        }
    }

    pub fn stop(&self) {
        if let Some(child) = self.child.lock().unwrap().take() {
            let _ = child.kill();
        }
    }
}
