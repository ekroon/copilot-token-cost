mod settings;
mod sidecar;

use crate::settings::Settings;
use crate::sidecar::SidecarManager;
use std::sync::Mutex;
use tauri::{
    menu::{CheckMenuItem, Menu, MenuId, MenuItem, PredefinedMenuItem, Submenu},
    tray::TrayIconBuilder,
    Manager,
};

struct AppState {
    settings: Mutex<Settings>,
    sidecar: SidecarManager,
    menu: Menu<tauri::Wry>,
}

const PERIOD_OPTIONS: &[(&str, &str)] = &[
    ("today", "Today"),
    ("yesterday", "Yesterday"),
    ("7", "7 days"),
    ("14", "14 days"),
    ("30", "30 days"),
    ("all", "All"),
];

const PERIOD_IDS: &[&str] = &[
    "period_today",
    "period_yesterday",
    "period_7",
    "period_14",
    "period_30",
    "period_all",
];

fn get_check_item(
    menu: &Menu<tauri::Wry>,
    id: &str,
) -> Option<CheckMenuItem<tauri::Wry>> {
    let menu_id = MenuId::new(id);
    menu.get(&menu_id).and_then(|item| item.as_check_menuitem().cloned())
}

fn set_radio_group(menu: &Menu<tauri::Wry>, ids: &[&str], selected_id: &str) {
    for id in ids {
        if let Some(item) = get_check_item(menu, id) {
            let _ = item.set_checked(*id == selected_id);
        }
    }
}

fn build_tray_menu(
    app: &tauri::AppHandle,
    settings: &Settings,
) -> tauri::Result<Menu<tauri::Wry>> {
    let show = MenuItem::with_id(app, "show", "Show Window", true, None::<&str>)?;
    let sep1 = PredefinedMenuItem::separator(app)?;

    let period_items: Vec<CheckMenuItem<tauri::Wry>> = PERIOD_OPTIONS
        .iter()
        .map(|(value, label)| {
            CheckMenuItem::with_id(
                app,
                format!("period_{}", value),
                *label,
                true,
                settings.period == *value,
                None::<&str>,
            )
            .unwrap()
        })
        .collect();
    let period_refs: Vec<&dyn tauri::menu::IsMenuItem<tauri::Wry>> =
        period_items.iter().map(|i| i as &dyn tauri::menu::IsMenuItem<tauri::Wry>).collect();
    let period_submenu = Submenu::with_items(app, "Period", true, &period_refs)?;

    let sep2 = PredefinedMenuItem::separator(app)?;

    let local_streaming = CheckMenuItem::with_id(
        app, "toggle_local_streaming", "Local Streaming",
        true, settings.local_streaming, None::<&str>,
    )?;
    let cs_streaming = CheckMenuItem::with_id(
        app, "toggle_codespaces_streaming", "Codespaces Streaming",
        true, settings.codespaces_streaming, None::<&str>,
    )?;

    let cs_manual = CheckMenuItem::with_id(
        app, "cs_mode_manual", "Manual",
        true, settings.codespaces_mode == "manual", None::<&str>,
    )?;
    let cs_auto = CheckMenuItem::with_id(
        app, "cs_mode_auto", "Auto",
        true, settings.codespaces_mode == "auto", None::<&str>,
    )?;
    let cs_mode_submenu = Submenu::with_items(
        app, "Codespaces Mode", true,
        &[
            &cs_manual as &dyn tauri::menu::IsMenuItem<tauri::Wry>,
            &cs_auto,
        ],
    )?;

    let cs_include_stopped = CheckMenuItem::with_id(
        app, "toggle_include_stopped", "Include Stopped Codespaces",
        true, settings.codespaces_include_stopped, None::<&str>,
    )?;

    let sep3 = PredefinedMenuItem::separator(app)?;
    let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;

    Menu::with_items(
        app,
        &[
            &show,
            &sep1,
            &period_submenu,
            &sep2,
            &local_streaming,
            &cs_streaming,
            &cs_mode_submenu,
            &cs_include_stopped,
            &sep3,
            &quit,
        ],
    )
}

fn handle_menu_event(app: &tauri::AppHandle, event_id: &str) {
    eprintln!("[desktop] menu event: {}", event_id);
    match event_id {
        "quit" => {
            let state = app.state::<AppState>();
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
            let state = app.state::<AppState>();
            set_radio_group(&state.menu, PERIOD_IDS, id);
            let mut s = state.settings.lock().unwrap();
            s.period = period.to_string();
            save_and_restart(app, &s, &state.sidecar);
        }
        "toggle_local_streaming" => {
            // Tauri auto-toggles the checkmark; just sync settings
            toggle_bool(app, |s| &mut s.local_streaming);
        }
        "toggle_codespaces_streaming" => {
            toggle_bool(app, |s| &mut s.codespaces_streaming);
        }
        "toggle_include_stopped" => {
            toggle_bool(app, |s| &mut s.codespaces_include_stopped);
        }
        "cs_mode_manual" => {
            set_codespaces_mode(app, "manual");
        }
        "cs_mode_auto" => {
            set_codespaces_mode(app, "auto");
        }
        _ => {
            eprintln!("[desktop] unhandled menu event: {}", event_id);
        }
    }
}

fn toggle_bool(app: &tauri::AppHandle, field: fn(&mut Settings) -> &mut bool) {
    let state = app.state::<AppState>();
    let mut s = state.settings.lock().unwrap();
    let val = field(&mut s);
    *val = !*val;
    save_and_restart(app, &s, &state.sidecar);
}

fn set_codespaces_mode(app: &tauri::AppHandle, mode: &str) {
    let state = app.state::<AppState>();
    set_radio_group(
        &state.menu,
        &["cs_mode_manual", "cs_mode_auto"],
        &format!("cs_mode_{}", mode),
    );
    let mut s = state.settings.lock().unwrap();
    s.codespaces_mode = mode.to_string();
    save_and_restart(app, &s, &state.sidecar);
}

fn save_and_restart(
    app: &tauri::AppHandle,
    settings: &Settings,
    sidecar: &SidecarManager,
) {
    if let Some(dir) = app.path().app_data_dir().ok() {
        if let Err(e) = settings.save(&dir) {
            eprintln!("[desktop] failed to save settings: {}", e);
        }
    }
    sidecar.restart(app, settings);
}

fn main() {
    tauri::Builder::default()
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

            let menu = build_tray_menu(&app.handle(), &settings)?;

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
                menu,
            });

            Ok(())
        })
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
