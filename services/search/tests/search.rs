use std::{
    env,
    path::PathBuf,
    time::{SystemTime, UNIX_EPOCH},
};

use lolcatz_search::{application, monitoring, Image, Metrics};
use rocket::{http::Status, local::asynchronous::Client};
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    Executor,
};

#[rocket::async_test]
async fn public_search_and_metrics_contract() {
    let url = env::var("TEST_DATABASE_URL")
        .expect("TEST_DATABASE_URL is required: run docker compose run --build --rm search-tests");
    let admin = PgPoolOptions::new().connect(&url).await.unwrap();
    let schema = format!(
        "search_test_{}",
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    );
    admin
        .execute(format!("CREATE SCHEMA {schema}").as_str())
        .await
        .unwrap();
    let options: PgConnectOptions = url.parse().unwrap();
    let pool = PgPoolOptions::new()
        .max_connections(2)
        .connect_with(options.options([("search_path", format!("{schema},public").as_str())]))
        .await
        .unwrap();
    let schema_path = match env::var("SEARCH_TEST_SCHEMA_DIR") {
        Ok(dir) => PathBuf::from(dir).join("uploader.sql"),
        Err(_) => PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../uploader/schema.sql"),
    };
    sqlx::raw_sql(&std::fs::read_to_string(schema_path).unwrap())
        .execute(&pool)
        .await
        .unwrap();
    sqlx::raw_sql(r#"
        INSERT INTO images (id,board,title,filename,content_type,uploaded_at) VALUES
            ('first','b','Cat portrait','first.jpg','image/jpeg','2025-01-01T00:00:00Z'),
            ('second','g','  ','second.png','image/png','2025-01-02T00:00:00Z'),
            ('third','b','Dog','third.jpg','image/jpeg','2025-01-03T00:00:00Z');
        INSERT INTO image_ocr (image_id,text,derived_title,producer_version) VALUES ('second','CAT on a laptop','A laptop cat','test');
        INSERT INTO image_annotations (image_id,label,confidence,x1,y1,x2,y2,producer_version) VALUES
            ('first','cat',0.8,0,0,1,1,'test'), ('first','cat',0.9,0.1,0.2,0.8,0.9,'test'),
            ('second','cat',0.7,0,0,1,1,'test');
        INSERT INTO comments (image_id,body) VALUES ('first','one'), ('first','two');
    "#).execute(&pool).await.unwrap();

    let metrics = Metrics::new().unwrap();
    let config = rocket::Config {
        log_level: rocket::config::LogLevel::Off,
        ..Default::default()
    };
    let client = Client::tracked(application(config.clone(), pool.clone(), metrics.clone()))
        .await
        .unwrap();
    let monitor = Client::tracked(monitoring(config, &metrics)).await.unwrap();

    for query in ["", "?q=%20%20", "?board=b"] {
        let response = client
            .get(format!("/api/search/search{query}"))
            .dispatch()
            .await;
        assert_eq!(response.status(), Status::BadRequest);
        assert_eq!(response.into_string().await.unwrap(), "provide q or tag\n");
    }
    // No token is supplied: public title/OCR search and whitespace trimming.
    let response = client
        .get("/api/search/search?q=%20cat%20")
        .dispatch()
        .await;
    assert_eq!(response.status(), Status::Ok);
    let images = response.into_json::<Vec<Image>>().await.unwrap();
    assert_eq!(
        images.iter().map(|i| i.id.as_str()).collect::<Vec<_>>(),
        ["second", "first"]
    );
    assert_eq!(images[0].title, "A laptop cat");
    assert_eq!(images[0].image_url, "/api/browse/media/second");
    assert_eq!(images[1].comment_count, 2);
    assert_eq!(images[1].tags.0.len(), 1);
    assert!((images[1].tags.0[0].confidence - 0.9).abs() < 0.001);
    assert_eq!(images[1].annotations.0.len(), 2);
    assert_eq!(images[1].annotations.0[0].bbox, [0.0, 0.0, 1.0, 1.0]);
    assert_eq!(
        images[1].uploaded_at.to_rfc3339(),
        "2025-01-01T00:00:00+00:00"
    );

    let images = client
        .get("/api/search/search?q=cat&board=b")
        .dispatch()
        .await
        .into_json::<Vec<Image>>()
        .await
        .unwrap();
    assert_eq!(images.len(), 1);
    assert_eq!(images[0].id, "first");
    // Preserve existing precedence: a tag takes precedence over both q and board.
    let images = client
        .get("/api/search/search?tag=%20cat%20&q=dog&board=absent")
        .dispatch()
        .await
        .into_json::<Vec<Image>>()
        .await
        .unwrap();
    assert_eq!(images.len(), 2);
    assert_eq!(
        client
            .get("/api/search/search?q=nomatch")
            .dispatch()
            .await
            .into_string()
            .await
            .unwrap(),
        "[]"
    );
    assert_eq!(
        client
            .get("/api/search/search?q=%27%20OR%201%3D1--")
            .dispatch()
            .await
            .into_string()
            .await
            .unwrap(),
        "[]"
    );
    let images = client
        .get("/api/search/search?q=dog")
        .dispatch()
        .await
        .into_json::<Vec<Image>>()
        .await
        .unwrap();
    assert!(images[0].tags.0.is_empty());
    assert!(images[0].annotations.0.is_empty());

    sqlx::raw_sql("INSERT INTO images (id,title,filename,content_type,uploaded_at) SELECT 'limit-' || n, 'limit-test', 'x.jpg', 'image/jpeg', '2025-01-01'::timestamptz + n * interval '1 second' FROM generate_series(1,55) n").execute(&pool).await.unwrap();
    let images = client
        .get("/api/search/search?q=limit-test")
        .dispatch()
        .await
        .into_json::<Vec<Image>>()
        .await
        .unwrap();
    assert_eq!(images.len(), 50);
    assert_eq!(images[0].id, "limit-55");
    assert_eq!(images[49].id, "limit-6");

    // A later, undecodable annotation must reject the entire result set.
    sqlx::raw_sql("ALTER TABLE image_annotations ALTER COLUMN label DROP NOT NULL; UPDATE image_annotations SET label=NULL WHERE image_id='first'").execute(&pool).await.unwrap();
    let response = client.get("/api/search/search?q=cat").dispatch().await;
    assert_eq!(response.status(), Status::InternalServerError);
    assert_eq!(response.into_string().await.unwrap(), "db error\n");
    sqlx::raw_sql("UPDATE image_annotations SET label='cat'; ALTER TABLE image_annotations ALTER COLUMN confidence DROP NOT NULL; UPDATE image_annotations SET confidence=NULL WHERE image_id='first'").execute(&pool).await.unwrap();
    assert_eq!(
        client
            .get("/api/search/search?q=cat")
            .dispatch()
            .await
            .status(),
        Status::InternalServerError
    );
    pool.close().await;
    assert_eq!(
        client
            .get("/api/search/search?q=cat")
            .dispatch()
            .await
            .status(),
        Status::InternalServerError
    );

    assert_eq!(
        client.get("/health").dispatch().await.status(),
        Status::NotFound
    );
    assert_eq!(
        client.get("/metrics").dispatch().await.status(),
        Status::NotFound
    );
    assert_eq!(monitor.get("/health").dispatch().await.status(), Status::Ok);
    assert_eq!(
        monitor
            .get("/api/search/search?q=cat")
            .dispatch()
            .await
            .status(),
        Status::NotFound
    );
    let response = monitor.get("/metrics").dispatch().await;
    assert_eq!(response.status(), Status::Ok);
    let text = response.into_string().await.unwrap();
    assert!(text.contains("# TYPE lolcatz_search_query_duration_seconds histogram"));
    assert!(text.contains(
        "lolcatz_search_query_duration_seconds_count{outcome=\"success\",query_type=\"text\"} 5"
    ));
    assert!(text.contains(
        "lolcatz_search_query_duration_seconds_count{outcome=\"error\",query_type=\"text\"} 3"
    ));
    assert!(text.contains("lolcatz_search_query_duration_seconds_count{outcome=\"success\",query_type=\"board_text\"} 1"));
    assert!(text.contains(
        "lolcatz_search_query_duration_seconds_count{outcome=\"success\",query_type=\"tag\"} 1"
    ));
    assert!(!text.contains("limit-test"));
    assert!(!text.contains("nomatch"));
    assert!(text.contains("lolcatz_http_requests_in_flight 0"));
    admin
        .execute(format!("DROP SCHEMA {schema} CASCADE").as_str())
        .await
        .unwrap();
}
