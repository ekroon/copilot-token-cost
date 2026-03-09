mod settings;
mod sidecar;

use crate::settings::Settings;
use crate::sidecar::SidecarManager;
use std::sync::Mutex;
use tauri::{
    menu::{Menu, MenuItem, PredefinedMenuItem, Submenu},
    tray::TrayIconBuilder,
    Manager, RunEvent,
};

struct AppState {
    settings: Mutex<Settings>,
    sidecar: SidecarManager,
    tray_items: TrayItems,
}

struct TrayItems {
    periods: Vec<(String, MenuItem<tauri::Wry>)>,
    local_streaming: MenuItem<tauri::Wry>,
    cs_streaming: MenuItem<tauri::Wry>,
    cs_manual_mode: MenuItem<tauri::Wry>,
    include_stopped: MenuItem<tauri::Wry>,
}

const PERIOD_OPTIONS: &[(&str, &str)] = &[
    ("today", "Today"),
    ("yesterday", "Yesterday"),
    ("7", "7 days"),
    ("14", "14 days"),
    ("30", "30 days"),
    ("all", "All"),
];

fn check_label(label: &str, checked: bool) -> String {
    if checked {
        format!("✓ {}", label)
    } else {
        format!("   {}", label)
    }
}

fn build_tray(
    app: &tauri::AppHandle,
    settings: &Settings,
) -> tauri::Result<(Menu<tauri::Wry>, TrayItems)> {
    let show = MenuItem::with_id(app, "show", "Show Window", true, None::<&str>)?;
    let sep1 = PredefinedMenuItem::separator(app)?;

    let mut periods = Vec::new();
    let mut period_menu_items: Vec<MenuItem<tauri::Wry>> = Vec::new();
    for (value, label) in PERIOD_OPTIONS {
        let item = MenuItem::with_id(
            app,
            format!("period_{}", value),
            check_label(label, settings.period == *value),
            true,
            None::<&str>,
        )?;
        periods.push((value.to_string(), item.clone()));
        period_menu_items.push(item);
    }
    let period_refs: Vec<&dyn tauri::menu::IsMenuItem<tauri::Wry>> = period_menu_items
        .iter()
        .map(|i| i as &dyn tauri::menu::IsMenuItem<tauri::Wry>)
        .collect();
    let period_submenu = Submenu::with_items(app, "Period", true, &period_refs)?;

    let sep2 = PredefinedMenuItem::separator(app)?;

    let local_streaming = MenuItem::with_id(
        app,
        "toggle_local_streaming",
        check_label("Local Streaming", settings.local_streaming),
        true,
        None::<&str>,
    )?;
    let cs_streaming = MenuItem::with_id(
        app,
        "toggle_cs_streaming",
        check_label("Codespaces Streaming", settings.codespaces_streaming),
        true,
        None::<&str>,
    )?;
    let cs_manual_mode = MenuItem::with_id(
        app,
        "toggle_cs_manual",
        check_label(
            "Manual Codespaces Mode",
            settings.codespaces_mode == "manual",
        ),
        true,
        None::<&str>,
    )?;
    let include_stopped = MenuItem::with_id(
        app,
        "toggle_include_stopped",
        check_label(
            "Include Stopped Codespaces",
            settings.codespaces_include_stopped,
        ),
        true,
        None::<&str>,
    )?;

    let sep3 = PredefinedMenuItem::separator(app)?;
    let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;

    let menu = Menu::with_items(
        app,
        &[
            &show,
            &sep1,
            &period_submenu,
            &sep2,
            &local_streaming,
            &cs_streaming,
            &cs_manual_mode,
            &include_stopped,
            &sep3,
            &quit,
        ],
    )?;

    let items = TrayItems {
        periods,
        local_streaming: local_streaming.clone(),
        cs_streaming: cs_streaming.clone(),
        cs_manual_mode: cs_manual_mode.clone(),
        include_stopped: include_stopped.clone(),
    };

    Ok((menu, items))
}

fn handle_menu_event(app: &tauri::AppHandle, event_id: &str) {
    eprintln!("[desktop] menu event: {}", event_id);
    let state = app.state::<AppState>();

    match event_id {
        "quit" => {
            state.sidecar.stop();
            app.exit(0);
        }
        "show" => {
            if let Some(w) = app.get_webview_window("main") {
                let _ = w.show();
                let _ = w.set_focus();
            }
        }
        id if id.starts_with("period_") => {
            let period = id.strip_prefix("period_").unwrap();
            for (val, item) in &state.tray_items.periods {
                let label = PERIOD_OPTIONS
                    .iter()
                    .find(|(v, _)| *v == val.as_str())
                    .map(|(_, l)| *l)
                    .unwrap_or(val.as_str());
                let _ = item.set_text(check_label(label, val == period));
            }
            let mut s = state.settings.lock().unwrap();
            s.period = period.to_string();
            save_and_restart(app, &s, &state.sidecar);
        }
        "toggle_local_streaming" => {
            let mut s = state.settings.lock().unwrap();
            s.local_streaming = !s.local_streaming;
            let _ = state
                .tray_items
                .local_streaming
                .set_text(check_label("Local Streaming", s.local_streaming));
            save_and_restart(app, &s, &state.sidecar);
        }
        "toggle_cs_streaming" => {
            let mut s = state.settings.lock().unwrap();
            s.codespaces_streaming = !s.codespaces_streaming;
            let _ = state
                .tray_items
                .cs_streaming
                .set_text(check_label("Codespaces Streaming", s.codespaces_streaming));
            save_and_restart(app, &s, &state.sidecar);
        }
        "toggle_cs_manual" => {
            let mut s = state.settings.lock().unwrap();
            s.codespaces_mode = if s.codespaces_mode == "manual" {
                "auto".to_string()
            } else {
                "manual".to_string()
            };
            let _ = state.tray_items.cs_manual_mode.set_text(check_label(
                "Manual Codespaces Mode",
                s.codespaces_mode == "manual",
            ));
            save_and_restart(app, &s, &state.sidecar);
        }
        "toggle_include_stopped" => {
            let mut s = state.settings.lock().unwrap();
            s.codespaces_include_stopped = !s.codespaces_include_stopped;
            let _ = state.tray_items.include_stopped.set_text(check_label(
                "Include Stopped Codespaces",
                s.codespaces_include_stopped,
            ));
            save_and_restart(app, &s, &state.sidecar);
        }
        _ => {
            eprintln!("[desktop] unhandled menu event: {}", event_id);
        }
    }
}

fn save_and_restart(app: &tauri::AppHandle, settings: &Settings, sidecar: &SidecarManager) {
    if let Some(dir) = app.path().app_data_dir().ok() {
        if let Err(e) = settings.save(&dir) {
            eprintln!("[desktop] failed to save settings: {}", e);
        }
    }
    sidecar.restart(app, settings);
}

fn stop_sidecar(app: &tauri::AppHandle) {
    if let Some(state) = app.try_state::<AppState>() {
        state.sidecar.stop();
    }
}

fn main() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .setup(|app| {
            let data_dir = app
                .path()
                .app_data_dir()
                .expect("failed to resolve app data dir");
            let settings = Settings::load(&data_dir);
            eprintln!("[desktop] loaded settings: {:?}", settings);

            let sidecar = SidecarManager::new();
            sidecar.spawn(&app.handle(), &settings);

            let (menu, tray_items) = build_tray(&app.handle(), &settings)?;

            TrayIconBuilder::with_id("main-tray")
                .icon(app.default_window_icon().unwrap().clone())
                .menu(&menu)
                .tooltip("Copilot Token Cost")
                .on_menu_event(|app, event| {
                    handle_menu_event(app, event.id.as_ref());
                })
                .build(app)?;

            app.manage(AppState {
                settings: Mutex::new(settings),
                sidecar,
                tray_items,
            });

            Ok(())
        })
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .build(tauri::generate_context!())
        .expect("error while building tauri application");

    app.run(|app, event| {
        if matches!(event, RunEvent::ExitRequested { .. } | RunEvent::Exit) {
            stop_sidecar(app);
        }
    });
}
