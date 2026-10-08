//! Локальное состояние: AppID, секрет устройства и токены.
//! Лежит в каталоге настроек пользователя, отдельно от бинарника, поэтому
//! переживает обновление и перезапуск.

use serde::{Deserialize, Serialize};
use std::{fs, io, path::PathBuf};

#[derive(Serialize, Deserialize, Default, Clone)]
pub struct Data {
    pub app_id: String,
    /// Известен только этому устройству; доказывает серверу, что /login
    /// поллит именно владелец AppID (сам AppID публичен — он в QR).
    pub device_secret: String,
    #[serde(default)]
    pub access_token: Option<String>,
    #[serde(default)]
    pub refresh_token: Option<String>,
}

pub struct Store {
    pub path: PathBuf,
    pub data: Data,
}

impl Store {
    pub fn load_or_init() -> io::Result<Self> {
        let dir = match std::env::var_os("SHOWCASE_DATA_DIR") {
            Some(d) => PathBuf::from(d),
            None => dirs::config_dir().unwrap_or_else(|| PathBuf::from(".")).join("showcase"),
        };
        fs::create_dir_all(&dir)?;
        let path = dir.join("state.json");
        let existing = fs::read(&path).ok().and_then(|b| serde_json::from_slice::<Data>(&b).ok());
        let mut store = Store { path, data: existing.unwrap_or_default() };
        if store.data.app_id.is_empty() || store.data.device_secret.is_empty() {
            // Первый запуск на этом ПК.
            store.data = Data {
                app_id: uuid::Uuid::new_v4().to_string(),
                device_secret: format!("{}{}", uuid::Uuid::new_v4().simple(), uuid::Uuid::new_v4().simple()),
                ..Default::default()
            };
            store.save()?;
            crate::log!("first run: generated app_id={}", store.data.app_id);
        }
        Ok(store)
    }

    /// Атомарная запись: tmp-файл + rename, чтобы сбой не оставил битый JSON.
    pub fn save(&self) -> io::Result<()> {
        let tmp = self.path.with_extension("json.tmp");
        fs::write(&tmp, serde_json::to_vec_pretty(&self.data)?)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&tmp, fs::Permissions::from_mode(0o600))?;
        }
        fs::rename(&tmp, &self.path)
    }
}
