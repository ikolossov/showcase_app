//! Самообновление: проверка версии, загрузка, проверка SHA-256, подмена
//! бинарника и перезапуск. Данные авторизации лежат отдельно (см. store.rs),
//! поэтому новый процесс стартует уже авторизованным.

use crate::api::{Api, Update};
use crate::log;
use crate::ui::State;
use sha2::{Digest, Sha256};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use std::{fs, io};

pub fn run(api: Api, exe: PathBuf, state: Arc<Mutex<State>>, ctx: eframe::egui::Context, every: Duration) {
    let set = |msg: String| {
        log!("update: {msg}");
        state.lock().unwrap().update = msg;
        ctx.request_repaint();
    };
    std::thread::sleep(Duration::from_secs(3));
    loop {
        match api.check_update() {
            Ok(None) => set(format!("Обновлений нет, версия {} актуальна", crate::VERSION)),
            Ok(Some(upd)) => {
                set(format!("Загружается версия {}…", upd.version));
                match install(&api, &upd, &exe) {
                    Ok(()) => {
                        set(format!("Установлена {}, перезапуск…", upd.version));
                        std::thread::sleep(Duration::from_millis(500));
                        let err = restart(&exe);
                        set(format!("Не удалось перезапуститься: {err}"));
                    }
                    Err(e) => set(format!("Ошибка обновления до {}: {e}", upd.version)),
                }
            }
            Err(e) => set(format!("Проверка обновлений: {e}")),
        }
        std::thread::sleep(every);
    }
}

fn install(api: &Api, upd: &Update, exe: &Path) -> io::Result<()> {
    // Временный файл рядом с бинарником — тот же раздел диска, rename атомарен.
    let tmp = exe.with_file_name(format!(".{}.new", exe.file_name().unwrap_or_default().to_string_lossy()));
    let result = (|| {
        let mut body = api.download(&upd.url).map_err(|e| io::Error::other(e.to_string()))?;
        let mut file = fs::File::create(&tmp)?;
        let mut hasher = Sha256::new();
        io::copy(&mut body, &mut HashWriter { inner: &mut file, hasher: &mut hasher })?;
        file.sync_all()?;
        drop(file);
        let got = hex(&hasher.finalize());
        if !got.eq_ignore_ascii_case(&upd.sha256) {
            return Err(io::Error::other(format!("sha256 не совпал: {got}")));
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&tmp, fs::Permissions::from_mode(0o755))?;
        }
        // На Windows работающий .exe нельзя перезаписать — self_replace
        // переименовывает его и удаляет старую копию после выхода.
        self_replace::self_replace(&tmp)
    })();
    let _ = fs::remove_file(&tmp);
    result
}

/// Перезапуск с теми же аргументами. Возвращается только при ошибке.
fn restart(exe: &Path) -> io::Error {
    let mut cmd = std::process::Command::new(exe);
    cmd.args(std::env::args_os().skip(1));
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        cmd.exec() // заменяем текущий процесс, PID сохраняется
    }
    #[cfg(not(unix))]
    {
        match cmd.spawn() {
            Ok(_) => std::process::exit(0),
            Err(e) => e,
        }
    }
}

struct HashWriter<'a, W> {
    inner: &'a mut W,
    hasher: &'a mut Sha256,
}

impl<W: io::Write> io::Write for HashWriter<'_, W> {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let n = self.inner.write(buf)?;
        self.hasher.update(&buf[..n]);
        Ok(n)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.inner.flush()
    }
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
