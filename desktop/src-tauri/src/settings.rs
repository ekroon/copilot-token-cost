use serde::{Deserialize, Serialize};
use std::fs;
use std::path::Path;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Settings {
    #[serde(default = "default_period")]
    pub period: String,
    #[serde(default = "default_true")]
    pub local_streaming: bool,
    #[serde(default = "default_codespaces_mode")]
    pub codespaces_mode: String,
    #[serde(default)]
    pub codespaces_streaming: bool,
    #[serde(default)]
    pub codespaces_include_stopped: bool,
}

fn default_period() -> String {
    "14".into()
}
fn default_true() -> bool {
    true
}
fn default_codespaces_mode() -> String {
    "manual".into()
}

impl Default for Settings {
    fn default() -> Self {
        Self {
            period: default_period(),
            local_streaming: default_true(),
            codespaces_mode: default_codespaces_mode(),
            codespaces_streaming: false,
            codespaces_include_stopped: false,
        }
    }
}

impl Settings {
    pub fn load(app_data_dir: &Path) -> Self {
        let path = app_data_dir.join("settings.json");
        match fs::read_to_string(&path) {
            Ok(contents) => serde_json::from_str(&contents).unwrap_or_default(),
            Err(_) => Self::default(),
        }
    }

    pub fn save(&self, app_data_dir: &Path) -> Result<(), String> {
        fs::create_dir_all(app_data_dir).map_err(|e| e.to_string())?;
        let json =
            serde_json::to_string_pretty(self).map_err(|e| e.to_string())?;
        fs::write(app_data_dir.join("settings.json"), json)
            .map_err(|e| e.to_string())
    }

    pub fn to_sidecar_args(&self) -> Vec<String> {
        let mut args = vec![
            "--web".into(),
            "--web-listen".into(),
            "127.0.0.1:7332".into(),
            "--web-log-mode".into(),
            "errors".into(),
        ];

        if self.local_streaming {
            args.push("--web-local-streaming".into());
        }

        args.push("--web-codespaces-mode".into());
        args.push(self.codespaces_mode.clone());

        if self.codespaces_streaming {
            args.push("--web-codespaces-streaming".into());
        }

        if self.codespaces_include_stopped {
            args.push("--codespaces-include-stopped".into());
        }

        // Period as positional arg (must be last for non-flag values)
        match self.period.as_str() {
            "today" => args.push("--today".into()),
            "yesterday" => args.push("--yesterday".into()),
            "all" => args.push("--all".into()),
            days => args.push(days.into()),
        }

        args
    }

}
