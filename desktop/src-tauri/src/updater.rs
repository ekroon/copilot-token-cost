use tauri::Manager;
use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
use tauri_plugin_updater::UpdaterExt;

pub fn check_for_update(app: &tauri::AppHandle, silent: bool) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let updater = match app.updater() {
            Ok(u) => u,
            Err(e) => {
                eprintln!("[desktop] updater not available: {}", e);
                return;
            }
        };

        let update = match updater.check().await {
            Ok(Some(update)) => update,
            Ok(None) => {
                if !silent {
                    show_info(&app, "No Update Available", "You're already on the latest version.");
                }
                return;
            }
            Err(e) => {
                eprintln!("[desktop] update check failed: {}", e);
                if !silent {
                    show_info(
                        &app,
                        "Update Check Failed",
                        &format!("Could not check for updates:\n{}", e),
                    );
                }
                return;
            }
        };

        eprintln!(
            "[desktop] update available: {} -> {}",
            update.current_version, update.version
        );

        let version = update.version.clone();
        let app_clone = app.clone();
        let (tx, rx) = tokio::sync::oneshot::channel::<bool>();
        std::thread::spawn(move || {
            let result = app_clone
                .dialog()
                .message(format!(
                    "Version {} is available.\nWould you like to update and restart?",
                    version
                ))
                .title("Update Available")
                .buttons(MessageDialogButtons::OkCancelCustom(
                    "Update".to_string(),
                    "Later".to_string(),
                ))
                .blocking_show();
            let _ = tx.send(result);
        });
        let confirmed = rx.await.unwrap_or(false);

        if !confirmed {
            return;
        }

        // Stop sidecar before replacing the app bundle
        let state = app.state::<crate::AppState>();
        state.sidecar.stop();

        eprintln!("[desktop] downloading update…");
        if let Err(e) = update.download_and_install(|_, _| {}, || {}).await {
            eprintln!("[desktop] update install failed: {}", e);
            show_info(
                &app,
                "Update Failed",
                &format!("The update could not be installed:\n{}", e),
            );
            // Restart sidecar so the app keeps working
            let settings = state.settings.lock().unwrap().clone();
            state.sidecar.spawn(&app, &settings);
            return;
        }

        eprintln!("[desktop] update installed, restarting…");
        app.restart();
    });
}

fn show_info(app: &tauri::AppHandle, title: &str, message: &str) {
    let app = app.clone();
    let title = title.to_string();
    let message = message.to_string();
    std::thread::spawn(move || {
        app.dialog().message(message).title(title).blocking_show();
    });
}
