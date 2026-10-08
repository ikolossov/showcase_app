//! Интерфейс: экран товара и экран регистрации с QR.

use crate::api::Product;
use eframe::egui::{self, Color32, CornerRadius, FontFamily, FontId, RichText, Sense, Vec2};
use qrcodegen::{QrCode, QrCodeEcc};
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

pub enum Screen {
    Connecting,
    Product { product: Product, at: Instant },
    Register { url: String },
}

pub struct State {
    pub screen: Screen,
    pub error: Option<String>,
    pub update: String,
    pub next_fetch: Option<Instant>,
    server: String,
    app_id: String,
}

impl State {
    pub fn new(server: &str, app_id: &str) -> Self {
        Self {
            screen: Screen::Connecting,
            error: None,
            update: "Проверка обновлений…".into(),
            next_fetch: None,
            server: server.into(),
            app_id: app_id.into(),
        }
    }
}

pub struct App {
    state: Arc<Mutex<State>>,
    wake: mpsc::Sender<()>,
    qr: Option<(String, QrCode)>,
    /// Высота карточки товара в прошлом кадре — для центрирования по вертикали.
    product_h: f32,
}

impl App {
    pub fn new(state: Arc<Mutex<State>>, wake: mpsc::Sender<()>) -> Self {
        Self { state, wake, qr: None, product_h: 0.0 }
    }
}

/// Размер макета в логических точках, под который рисуется интерфейс.
/// На больших экранах всё пропорционально увеличивается.
const DESIGN: Vec2 = Vec2::new(640.0, 600.0);
const CONTENT_MAX_W: f32 = 720.0;

const ACCENT: Color32 = Color32::from_rgb(0x1a, 0x7f, 0x5a);

/// Только один шрифт (Ubuntu Light, есть кириллица) вместо полного набора
/// egui с emoji — экономит ~1 МБ в бинарнике.
pub fn setup_fonts(ctx: &egui::Context) {
    let mut fonts = egui::FontDefinitions::empty();
    fonts
        .font_data
        .insert("ubuntu".into(), Arc::new(egui::FontData::from_static(epaint_default_fonts::UBUNTU_LIGHT)));
    for family in [FontFamily::Proportional, FontFamily::Monospace] {
        fonts.families.insert(family, vec!["ubuntu".into()]);
    }
    ctx.set_fonts(fonts);
    ctx.set_theme(egui::Theme::Light);
}

impl eframe::App for App {
    fn ui(&mut self, ui: &mut egui::Ui, _frame: &mut eframe::Frame) {
        // Раз в секунду перерисовываем «N с назад» и обратный отсчёт.
        ui.ctx().request_repaint_after(Duration::from_secs(1));
        handle_keys(ui.ctx());
        auto_zoom(ui.ctx());
        let state = self.state.clone();
        let s = state.lock().unwrap();

        egui::Panel::bottom("status").show(ui, |ui| {
            ui.add_space(4.0);
            ui.label(RichText::new(&s.update).small());
            ui.label(
                RichText::new(format!("v{} · AppID {} · {}", crate::VERSION, s.app_id, s.server))
                    .small()
                    .weak(),
            );
            ui.add_space(4.0);
        });

        egui::CentralPanel::default().show(ui, |ui| {
            if let Some(err) = &s.error {
                ui.colored_label(Color32::from_rgb(0xc0, 0x36, 0x2c), format!("Нет связи с сервером: {err}"));
                ui.add_space(6.0);
            }
            match &s.screen {
                Screen::Connecting => {
                    ui.centered_and_justified(|ui| ui.spinner());
                }
                Screen::Product { product, at } => self.product(ui, product, *at, s.next_fetch),
                Screen::Register { url } => self.register(ui, url, &s.app_id),
            }
        });
    }
}

impl App {
    fn product(&mut self, ui: &mut egui::Ui, p: &Product, at: Instant, next: Option<Instant>) {
        egui::ScrollArea::vertical().show(ui, |ui| {
            let w = ui.available_width().min(CONTENT_MAX_W);
            ui.add_space(((ui.available_height() - self.product_h) / 2.0).max(8.0));
            ui.horizontal(|ui| {
                ui.add_space((ui.available_width() - w) / 2.0);
                let card = ui.vertical(|ui| {
                    ui.set_width(w);
                    self.product_card(ui, p, at, next);
                });
                self.product_h = card.response.rect.height();
            });
        });
    }

    fn product_card(&self, ui: &mut egui::Ui, p: &Product, at: Instant, next: Option<Instant>) {
        ui.label(RichText::new(&p.name).size(22.0).strong());
        ui.label(RichText::new(format!("Артикул {}", p.sku)).weak());
        ui.add_space(12.0);
        ui.horizontal(|ui| {
            ui.label(RichText::new(money(p.price, &p.currency)).size(34.0).color(ACCENT).strong());
            if let Some(old) = p.old_price.filter(|&o| o > p.price) {
                ui.label(RichText::new(money(old, &p.currency)).size(18.0).weak().strikethrough());
            }
        });
        ui.label(format!("В наличии: {} шт.", p.in_stock));
        ui.add_space(16.0);
        ui.label(RichText::new("Характеристики").size(16.0).strong());
        ui.add_space(4.0);
        egui::Grid::new("specs").striped(true).num_columns(2).spacing([24.0, 8.0]).show(ui, |ui| {
            for spec in &p.specs {
                ui.label(RichText::new(&spec.name).weak());
                ui.label(&spec.value);
                ui.end_row();
            }
        });
        ui.add_space(16.0);
        let next = next.map(|n| n.saturating_duration_since(Instant::now()).as_secs()).unwrap_or(0);
        ui.label(
            RichText::new(format!(
                "Цена на сервере на {} · получено {} с назад · следующий запрос через {}:{:02}",
                p.updated_at,
                at.elapsed().as_secs(),
                next / 60,
                next % 60
            ))
            .small()
            .weak(),
        );
        ui.add_space(8.0);
        if ui.button("Обновить сейчас").clicked() {
            let _ = self.wake.send(());
        }
    }

    fn register(&mut self, ui: &mut egui::Ui, url: &str, app_id: &str) {
        if self.qr.as_ref().is_none_or(|(u, _)| u != url) {
            self.qr = QrCode::encode_text(url, QrCodeEcc::Medium).ok().map(|q| (url.to_string(), q));
        }
        ui.vertical_centered(|ui| {
            ui.add_space(8.0);
            ui.label(RichText::new("Устройство не зарегистрировано").size(22.0).strong());
            ui.label("Отсканируйте QR-код телефоном и подтвердите привязку");
            ui.add_space(12.0);
            if let Some((_, qr)) = &self.qr {
                let side = (ui.available_height() - 110.0).min(ui.available_width()).clamp(160.0, 420.0);
                draw_qr(ui, qr, side);
            }
            ui.add_space(10.0);
            ui.add(egui::Label::new(RichText::new(url).font(FontId::proportional(13.0))).selectable(true));
            ui.label(RichText::new(format!("AppID: {app_id}")).small().weak());
            ui.add_space(8.0);
            ui.horizontal(|ui| {
                ui.add_space((ui.available_width() - 220.0).max(0.0) / 2.0);
                ui.spinner();
                ui.label("Ожидаем подтверждения…");
            });
        });
    }
}

/// F11 — переключить полноэкранный режим, Esc — выйти из него.
fn handle_keys(ctx: &egui::Context) {
    let (f11, esc, fullscreen) = ctx.input(|i| {
        (i.key_pressed(egui::Key::F11), i.key_pressed(egui::Key::Escape), i.viewport().fullscreen.unwrap_or(false))
    });
    if f11 || (esc && fullscreen) {
        ctx.send_viewport_cmd(egui::ViewportCommand::Fullscreen(f11 && !fullscreen));
    }
}

/// Масштаб интерфейса под размер окна: макет DESIGN вписывается в экран.
fn auto_zoom(ctx: &egui::Context) {
    let zoom = ctx.zoom_factor();
    let size = ctx.content_rect().size() * zoom; // размер окна при zoom = 1
    let target = (size.x / DESIGN.x).min(size.y / DESIGN.y).clamp(1.0, 4.0);
    if (target - zoom).abs() > 0.02 {
        ctx.set_zoom_factor(target);
    }
}

fn draw_qr(ui: &mut egui::Ui, qr: &QrCode, side: f32) {
    const QUIET: i32 = 4; // обязательная «тихая зона» по краям
    let n = qr.size() + QUIET * 2;
    let (rect, _) = ui.allocate_exact_size(Vec2::splat(side), Sense::hover());
    let painter = ui.painter_at(rect);
    painter.rect_filled(rect, CornerRadius::same(6), Color32::WHITE);
    let cell = side / n as f32;
    for y in 0..qr.size() {
        for x in 0..qr.size() {
            if qr.get_module(x, y) {
                let min = rect.min + Vec2::new((x + QUIET) as f32, (y + QUIET) as f32) * cell;
                // +0.5 px перекрытия убирает щели между модулями при дробном масштабе
                painter.rect_filled(egui::Rect::from_min_size(min, Vec2::splat(cell + 0.5)), 0, Color32::BLACK);
            }
        }
    }
}

/// 649990 → «649 990 тг»
fn money(v: i64, currency: &str) -> String {
    let digits = v.abs().to_string();
    let mut out = String::new();
    for (i, c) in digits.chars().enumerate() {
        if i > 0 && (digits.len() - i).is_multiple_of(3) {
            out.push(' ');
        }
        out.push(c);
    }
    format!("{}{out} {currency}", if v < 0 { "-" } else { "" })
}

#[cfg(test)]
mod tests {
    use super::money;

    #[test]
    fn money_groups_thousands() {
        assert_eq!(money(0, "тг"), "0 тг");
        assert_eq!(money(990, "тг"), "990 тг");
        assert_eq!(money(649_990, "тг"), "649 990 тг");
        assert_eq!(money(1_249_990, "тг"), "1 249 990 тг");
        assert_eq!(money(-5_000, "тг"), "-5 000 тг");
    }
}
