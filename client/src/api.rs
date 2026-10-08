//! HTTP-клиент к серверу.

use serde::Deserialize;
use std::{io::Read, time::Duration};
use ureq::Agent;

#[derive(Deserialize, Clone)]
pub struct Product {
    pub name: String,
    pub sku: String,
    pub price: i64,
    pub old_price: Option<i64>,
    pub currency: String,
    pub in_stock: i64,
    pub specs: Vec<Spec>,
    pub updated_at: String,
}

#[derive(Deserialize, Clone)]
pub struct Spec {
    pub name: String,
    pub value: String,
}

#[derive(Deserialize)]
pub struct Tokens {
    pub access_token: String,
    pub refresh_token: String,
}

#[derive(Deserialize)]
pub struct Update {
    pub version: String,
    pub url: String,
    pub sha256: String,
}

pub enum Error {
    Unauthorized,
    Other(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Unauthorized => f.write_str("401 Unauthorized"),
            Error::Other(e) => f.write_str(e),
        }
    }
}

impl From<ureq::Error> for Error {
    fn from(e: ureq::Error) -> Self {
        Error::Other(e.to_string())
    }
}

type Response = ureq::http::Response<ureq::Body>;

#[derive(Clone)]
pub struct Api {
    base: String,
    app_id: String,
    agent: Agent,
}

impl Api {
    pub fn new(base: &str, app_id: &str) -> Self {
        let agent = Agent::config_builder()
            .timeout_global(Some(Duration::from_secs(15)))
            .http_status_as_error(false)
            .user_agent(format!("showcase/{}", crate::VERSION))
            .build()
            .into();
        Self { base: base.into(), app_id: app_id.into(), agent }
    }

    pub fn register_url(&self) -> String {
        format!("{}/register?app_id={}", self.base, self.app_id)
    }

    fn get(&self, path: &str) -> ureq::RequestBuilder<ureq::typestate::WithoutBody> {
        let url = if path.starts_with("http") { path.to_string() } else { format!("{}{path}", self.base) };
        self.with_headers(self.agent.get(url))
    }

    fn post(&self, path: &str) -> ureq::RequestBuilder<ureq::typestate::WithBody> {
        self.with_headers(self.agent.post(format!("{}{path}", self.base)))
    }

    fn with_headers<B>(&self, r: ureq::RequestBuilder<B>) -> ureq::RequestBuilder<B> {
        r.header("X-App-ID", &self.app_id)
            .header("X-App-Version", crate::VERSION)
            .header("X-App-Platform", format!("{}-{}", std::env::consts::OS, std::env::consts::ARCH))
    }

    pub fn product(&self, access: &str) -> Result<Product, Error> {
        let resp = self.get("/api/v1/product").header("Authorization", format!("Bearer {access}")).call()?;
        json(resp)
    }

    pub fn refresh(&self, refresh: &str) -> Result<Tokens, Error> {
        let resp = self.post("/api/v1/token/refresh").send_json(serde_json::json!({ "refresh_token": refresh }))?;
        json(resp)
    }

    /// Ok(None) — устройство ещё не привязано, продолжаем поллить.
    pub fn login(&self, device_secret: &str) -> Result<Option<Tokens>, Error> {
        let resp = self
            .post("/api/v1/login")
            .send_json(serde_json::json!({ "app_id": self.app_id, "device_secret": device_secret }))?;
        if resp.status() == 202 {
            return Ok(None);
        }
        json(resp).map(Some)
    }

    pub fn check_update(&self) -> Result<Option<Update>, Error> {
        let resp = self
            .get("/api/v1/update")
            .query("os", std::env::consts::OS)
            .query("arch", std::env::consts::ARCH)
            .query("version", crate::VERSION)
            .call()?;
        if resp.status() == 204 {
            return Ok(None);
        }
        json(resp).map(Some)
    }

    /// Потоковая загрузка (без буферизации всего файла в памяти). URL может быть
    /// абсолютным (GitHub Releases) — туда идём без своих заголовков с AppID.
    pub fn download(&self, url: &str) -> Result<impl Read, Error> {
        let foreign = url.starts_with("http") && !url.starts_with(&format!("{}/", self.base));
        let req = if foreign { self.agent.get(url) } else { self.get(url) };
        let resp = req.config().timeout_global(Some(Duration::from_secs(600))).build().call()?;
        if !resp.status().is_success() {
            return Err(Error::Other(format!("download: HTTP {}", resp.status())));
        }
        Ok(resp.into_body().into_reader())
    }
}

fn json<T: serde::de::DeserializeOwned>(mut resp: Response) -> Result<T, Error> {
    match resp.status().as_u16() {
        200 => resp.body_mut().read_json().map_err(Into::into),
        401 => Err(Error::Unauthorized),
        code => Err(Error::Other(format!("HTTP {code}: {}", resp.body_mut().read_to_string().unwrap_or_default()))),
    }
}
