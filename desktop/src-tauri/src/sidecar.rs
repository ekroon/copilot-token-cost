use crate::settings::Settings;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;
use tauri::Manager;
use tauri_plugin_shell::process::CommandChild;
use tauri_plugin_shell::ShellExt;

pub struct SidecarManager {
    child: Arc<Mutex<Option<CommandChild>>>,
}

const RESTART_JS: &str = r#"
document.body.style.cssText = 'margin:0;display:flex;align-items:center;justify-content:center;height:100vh;background:#0d1117;color:#8b949e;font-family:system-ui';
document.body.innerHTML = '<p>Restarting…</p>';
const poll = setInterval(async () => {
    try {
        const r = await fetch('http://127.0.0.1:7332/healthz', {mode:'no-cors'});
        clearInterval(poll);
        window.location.replace('http://127.0.0.1:7332');
    } catch(e) {}
}, 300);
"#;

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
        // Show "Restarting…" immediately, then poll /healthz
        if let Some(w) = app.get_webview_window("main") {
            let _ = w.eval(RESTART_JS);
            let _ = w.show();
            let _ = w.set_focus();
        }

        self.stop();
        // Brief pause to let the OS release the port
        thread::sleep(Duration::from_millis(200));
        self.spawn(app, settings);
    }

    pub fn stop(&self) {
        if let Some(child) = self.child.lock().unwrap().take() {
            let _ = child.kill();
        }
    }
}
