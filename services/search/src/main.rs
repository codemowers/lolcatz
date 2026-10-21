use std::{env, error::Error, net::Ipv6Addr, time::Duration};

use lolcatz_search::{application, monitoring, Metrics};
use rocket::{config::TlsConfig, tokio};
use sqlx::postgres::PgPoolOptions;

#[rocket::main]
async fn main() -> Result<(), Box<dyn Error>> {
    if env::args().nth(1).as_deref() == Some("--healthcheck") {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        tokio::time::timeout(Duration::from_secs(2), async {
            let mut stream = tokio::net::TcpStream::connect("127.0.0.1:9090").await?;
            stream
                .write_all(b"GET /health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                .await?;
            let mut status = [0; 12];
            stream.read_exact(&mut status).await?;
            if &status != b"HTTP/1.1 200" {
                return Err(std::io::Error::other("unhealthy metrics listener"));
            }
            Ok::<_, std::io::Error>(())
        })
        .await??;
        return Ok(());
    }
    let database_url = env::var("DATABASE_URL")?;
    let pool = tokio::time::timeout(Duration::from_secs(120), async {
        let mut attempt = 0;
        loop {
            attempt += 1;
            match PgPoolOptions::new()
                .max_connections(10)
                .acquire_timeout(Duration::from_secs(5))
                .connect(&database_url)
                .await
            {
                Ok(pool) => return Ok(pool),
                Err(error) => {
                    let retryable = match &error {
                        sqlx::Error::Io(_) | sqlx::Error::PoolTimedOut => true,
                        sqlx::Error::Database(error) => error.code().is_some_and(|code| {
                            code.starts_with("08") || code == "57P03" || code == "53300"
                        }),
                        _ => false,
                    };
                    if !retryable {
                        return Err(error);
                    }
                    eprintln!("database not ready (attempt {attempt}); retrying in 2s: {error}");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
            }
        }
    })
    .await
    .map_err(|_| "database startup retry budget exhausted after 120s")??;
    let metrics = Metrics::new()?;
    let mut config = rocket::Config::from(rocket::Config::figment());
    config.port = env::var("PORT").unwrap_or_else(|_| "8080".into()).parse()?;
    let cert = env::var("TLS_CERT_FILE").ok().filter(|s| !s.is_empty());
    let key = env::var("TLS_KEY_FILE").ok().filter(|s| !s.is_empty());
    let tls_files = match (cert, key) {
        (Some(cert), Some(key)) => {
            let cert_bytes = tokio::fs::read(&cert).await?;
            let key_bytes = tokio::fs::read(&key).await?;
            config.tls = Some(TlsConfig::from_bytes(&cert_bytes, &key_bytes));
            Some((cert, key, cert_bytes, key_bytes))
        }
        (None, None) => None,
        _ => return Err("TLS_CERT_FILE and TLS_KEY_FILE must be configured together".into()),
    };
    let mut metrics_config = config.clone();
    metrics_config.address = Ipv6Addr::UNSPECIFIED.into();
    metrics_config.port = 9090;
    metrics_config.tls = None;
    let app = application(config, pool.clone(), metrics.clone())
        .ignite()
        .await?;
    let monitor = monitoring(metrics_config, &metrics).ignite().await?;
    let app_shutdown = app.shutdown();
    let monitor_shutdown = monitor.shutdown();
    // Rocket 0.5 loads TLS at startup. Drain and let Kubernetes/Compose restart
    // after a projected Secret rotates, so renewed certificates take effect.
    let watcher = tokio::spawn({
        let shutdown = app_shutdown.clone();
        async move {
            if let Some((cert, key, old_cert, old_key)) = tls_files {
                loop {
                    tokio::time::sleep(Duration::from_secs(30)).await;
                    if let (Ok(new_cert), Ok(new_key)) =
                        (tokio::fs::read(&cert).await, tokio::fs::read(&key).await)
                    {
                        if new_cert != old_cert || new_key != old_key {
                            eprintln!("TLS files changed; draining for restart");
                            shutdown.notify();
                            break;
                        }
                    }
                }
            }
        }
    });
    // A bind failure or shutdown of either listener must stop the whole service.
    let app_run = app.launch();
    let monitor_run = monitor.launch();
    tokio::pin!(app_run, monitor_run);
    let result = tokio::select! {
        result = &mut app_run => { monitor_shutdown.notify(); let other = monitor_run.await; result.and(other) },
        result = &mut monitor_run => { app_shutdown.notify(); let other = app_run.await; result.and(other) },
    };
    watcher.abort();
    pool.close().await;
    result?;
    Ok(())
}
