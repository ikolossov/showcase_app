//! Фоновый цикл: опрос товара, обновление токенов и регистрация по QR.

use crate::api::{Api, Error, Product, Tokens};
use crate::store::Store;
use crate::ui::{Screen, State};
use crate::log;
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

pub fn run(
    api: Api,
    mut store: Store,
    state: Arc<Mutex<State>>,
    ctx: eframe::egui::Context,
    wake: mpsc::Receiver<()>,
    product_every: Duration,
    login_every: Duration,
) {
    loop {
        let wait = match fetch_product(&api, &mut store) {
            Ok(product) => {
                log!("product: price={} {}", product.price, product.currency);
                let mut s = state.lock().unwrap();
                s.screen = Screen::Product { product, at: Instant::now() };
                s.error = None;
                product_every
            }
            Err(Error::Unauthorized) => {
                log!("401 → показываем QR и поллим /login");
                register(&api, &mut store, &state, &ctx, login_every);
                continue; // токены получены — сразу запрашиваем товар
            }
            Err(Error::Other(e)) => {
                log!("product error: {e}");
                state.lock().unwrap().error = Some(e);
                product_every.min(Duration::from_secs(15)) // при сетевой ошибке повторяем чаще
            }
        };
        state.lock().unwrap().next_fetch = Some(Instant::now() + wait);
        ctx.request_repaint();
        // Ждём интервал или нажатие «Обновить сейчас». Disconnected — окно закрыто.
        if let Err(mpsc::RecvTimeoutError::Disconnected) = wake.recv_timeout(wait) {
            return;
        }
    }
}

/// Запрос товара. На 401 один раз пробуем обновить токены по refresh.
fn fetch_product(api: &Api, store: &mut Store) -> Result<Product, Error> {
    let access = store.data.access_token.clone().unwrap_or_default();
    match api.product(&access) {
        Err(Error::Unauthorized) => {
            let Some(refresh) = store.data.refresh_token.clone() else {
                return Err(Error::Unauthorized);
            };
            log!("access token отклонён, обновляем по refresh");
            let tokens = api.refresh(&refresh)?;
            save_tokens(store, tokens);
            api.product(store.data.access_token.as_deref().unwrap_or_default())
        }
        r => r,
    }
}

/// Показывает QR и поллит /login, пока сервер не выдаст пару токенов.
fn register(api: &Api, store: &mut Store, state: &Mutex<State>, ctx: &eframe::egui::Context, every: Duration) {
    store.data.access_token = None;
    store.data.refresh_token = None;
    let _ = store.save();
    {
        let mut s = state.lock().unwrap();
        s.screen = Screen::Register { url: api.register_url() };
        s.next_fetch = None;
    }
    ctx.request_repaint();
    loop {
        match api.login(&store.data.device_secret) {
            Ok(Some(tokens)) => {
                log!("получена пара access/refresh");
                save_tokens(store, tokens);
                state.lock().unwrap().error = None;
                return;
            }
            Ok(None) => state.lock().unwrap().error = None,
            Err(e) => state.lock().unwrap().error = Some(format!("/login: {e}")),
        }
        ctx.request_repaint();
        std::thread::sleep(every);
    }
}

fn save_tokens(store: &mut Store, t: Tokens) {
    store.data.access_token = Some(t.access_token);
    store.data.refresh_token = Some(t.refresh_token);
    if let Err(e) = store.save() {
        log!("не удалось сохранить токены: {e}");
    }
}
