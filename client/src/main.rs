// В release-сборке под Windows не открываем консольное окно.
#![cfg_attr(all(windows, not(debug_assertions)), windows_subsystem = "windows")]

mod api;
mod store;
mod ui;
mod updater;
mod worker;

use std::sync::{Arc, Mutex, mpsc};
use std::time::Duration;

/// Версия зашивается при сборке: APP_VERSION=1.2.3 cargo build.
pub const VERSION: &str = match option_env!("APP_VERSION") {
    Some(v) => v,
    None => env!("CARGO_PKG_VERSION"),
};

/// Адрес сервера по умолчанию тоже можно зашить при сборке.
const DEFAULT_SERVER: &str = match option_env!("SHOWCASE_DEFAULT_SERVER") {
    Some(v) => v,
    None => "http://localhost:8080",
};

#[macro_export]
macro_rules! log {
    ($($arg:tt)*) => { eprintln!("[showcase {}] {}", $crate::VERSION, format_args!($($arg)*)) };
}

pub struct Config {
    pub server: String,
    pub product_every: Duration,
    pub update_every: Duration,
    pub login_every: Duration,
}

impl Config {
    fn from_env(server: String) -> Self {
        let secs = |key: &str, def: u64| {
            Duration::from_secs(std::env::var(key).ok().and_then(|v| v.parse().ok()).unwrap_or(def))
        };
        Self {
            server,
            product_every: secs("SHOWCASE_PRODUCT_INTERVAL", 600),
            update_every: secs("SHOWCASE_UPDATE_INTERVAL", 600),
            login_every: secs("SHOWCASE_LOGIN_INTERVAL", 2),
        }
    }
}

/// Адрес сервера: SHOWCASE_SERVER > сохранённый при первом запуске > вшитый при сборке.
/// Закрепляем его в state.json, чтобы обновление, собранное с другим
/// SHOWCASE_DEFAULT_SERVER (например, в CI), не переключило устройство на чужой сервер.
fn resolve_server(store: &mut store::Store) -> String {
    let server = std::env::var("SHOWCASE_SERVER")
        .ok()
        .or_else(|| store.data.server.clone())
        .unwrap_or_else(|| DEFAULT_SERVER.into())
        .trim_end_matches('/')
        .to_string();
    if store.data.server.as_deref() != Some(server.as_str()) {
        store.data.server = Some(server.clone());
        if let Err(e) = store.save() {
            log!("не удалось сохранить адрес сервера: {e}");
        }
    }
    server
}

fn main() -> eframe::Result {
    // Путь к бинарнику запоминаем до возможной подмены файла обновлением.
    let exe = std::env::current_exe().expect("current_exe");
    let mut store = store::Store::load_or_init().expect("не удалось открыть хранилище состояния");
    let cfg = Config::from_env(resolve_server(&mut store));
    log!("server={} app_id={} data={}", cfg.server, store.data.app_id, store.path.display());

    let api = api::Api::new(&cfg.server, &store.data.app_id);
    let state = Arc::new(Mutex::new(ui::State::new(&cfg.server, &store.data.app_id)));
    let (wake_tx, wake_rx) = mpsc::channel();

    // По умолчанию — во весь экран (F11 / Esc переключают). SHOWCASE_WINDOWED=1 — окно.
    let fullscreen = std::env::var_os("SHOWCASE_WINDOWED").is_none();
    let options = eframe::NativeOptions {
        viewport: eframe::egui::ViewportBuilder::default()
            .with_title(format!("Showcase {VERSION}"))
            .with_inner_size([640.0, 700.0])
            .with_min_inner_size([420.0, 520.0])
            .with_fullscreen(fullscreen),
        ..Default::default()
    };
    eframe::run_native(
        "showcase",
        options,
        Box::new(move |cc| {
            ui::setup_fonts(&cc.egui_ctx);
            let ctx = cc.egui_ctx.clone();
            {
                let (api, state, ctx) = (api.clone(), state.clone(), ctx.clone());
                let (product_every, login_every) = (cfg.product_every, cfg.login_every);
                std::thread::spawn(move || {
                    worker::run(api, store, state, ctx, wake_rx, product_every, login_every)
                });
            }
            {
                let (api, state, every) = (api.clone(), state.clone(), cfg.update_every);
                std::thread::spawn(move || updater::run(api, exe, state, ctx, every));
            }
            Ok(Box::new(ui::App::new(state, wake_tx)))
        }),
    )
}
