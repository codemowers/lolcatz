"use client";

import { signIn, signOut, useSession } from "next-auth/react";
import { useSearchParams, useRouter } from "next/navigation";
import { Suspense, useState } from "react";
import { usePreferences } from "../../lib/preferences";
import { TimeStamp } from "../../lib/time";
import { Post, PostCard } from "../post-card";

import { useAdminAccess } from "../../lib/admin-access";
import { AdminStatus } from "../admin-status";
import { AdminLink } from "../admin-link";
import { useResource } from "../../lib/use-resource";
import { request, errorMessage } from "../../lib/api";
import { LoadError } from "../load-error";

type Section = "account" | "settings" | "cli" | "uploads";

const sections: { id: Section; label: string }[] = [
  { id: "account", label: "Account" },
  { id: "uploads", label: "Uploads" },
  { id: "settings", label: "Settings" },
  { id: "cli", label: "Bulk upload" },
];

export default function ProfilePage() {
  return <Suspense fallback={<p className="empty">Loading profile…</p>}><ProfileContent /></Suspense>;
}

function ProfileContent() {
  const { data: session, status } = useSession();
  const devToken = process.env.NEXT_PUBLIC_DEV_AUTH_TOKEN;
  const token = devToken || session?.accessToken;
  const attributes = session?.oidcAttributes ?? {};
  const { theme, clock, setTheme, setClock } = usePreferences();

  const { status: adminStatus } = useAdminAccess();
  const params = useSearchParams();
  const router = useRouter();
  const selected = params.get("section");
  const section = sections.find(item => item.id === selected)?.id ?? "account";
  const { data: images = [], loading, error: loadError, reload } = useResource<Post[]>(token ? "/api/upload/me/images" : null, token);
  const [error, setError] = useState("");
  const [deletingId, setDeletingId] = useState<string | null>(null);

  const deletePost = async (image: Post) => {
    if (!token || !window.confirm(`Delete “${image.title || image.filename}” and all of its replies?`)) return;
    setDeletingId(image.id);
    setError("");
    try {
      await request(`/api/upload/images/${image.id}`, {
        method: "DELETE",
        headers: { Authorization: `Bearer ${token}` },
      });
      reload();
      window.dispatchEvent(new Event("boards-changed"));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setDeletingId(null);
    }
  };

  if (status === "loading" && !devToken) return <p className="empty">Checking login…</p>;

  if (status !== "authenticated" && !devToken) {
    return (
      <div className="panel auth-prompt">
        <h3>Your profile</h3>
        <p>Sign in to see your account attributes and uploaded images.</p>
        <button className="btn" type="button" onClick={() => signIn("passmower", { callbackUrl: "/profile" })}>
          Sign in
        </button>
      </div>
    );
  }

  const rows: Record<string, unknown> = devToken
    ? { email: "developer@localhost", environment: "local development" }
    : attributes;

  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const snippet = `for j in *.jpg *.jpeg *.png *.webp; do
  [ -f "$j" ] || continue
  curl --fail --progress-bar -T "$j" -H "Authorization: Bearer ${token}" "${origin}/api/upload/bulk/"
done`;

  return (
    <div>
      <div className="page-head">
        <h2 className="page-title">Your profile</h2>
        <AdminLink />
      </div>

      <div className="segmented" role="tablist">
        {sections.map(item => (
          <button className="btn btn-toggle" key={item.id} role="tab" type="button"
                  aria-selected={section === item.id}
                  onClick={() => router.push(`/profile?section=${item.id}`)}>
            {item.label}
          </button>
        ))}
      </div>

      {section === "account" && (
        <section className="panel">
          <div className="identity">
            {session?.user?.image && <img src={session.user.image} alt="Your avatar" />}
            <div>
              <div className="identity-name">{session?.user?.name ?? "Developer"}</div>
              <div className="identity-mail">{session?.user?.email ?? "developer@localhost"}</div>
            </div>
          </div>
          <p>Role: <strong>{adminStatus === "allowed" ? "Admin" : adminStatus === "denied" ? "Member" : "Not verified"}</strong></p>
          <AdminStatus />
          <table className="attributes"><tbody>
            {Object.entries(rows)
              .filter(([, value]) => value !== undefined && value !== "")
              .map(([key, value]) => (
                <tr key={key}>
                  <th>{key}</th>
                  <td>{Array.isArray(value) ? value.join(", ") : String(value)}</td>
                </tr>
              ))}
          </tbody></table>
          {!devToken && (
            <button className="btn btn-ghost" type="button" style={{ marginTop: 18 }}
                    onClick={() => signOut({ callbackUrl: "/" })}>
              Sign out
            </button>
          )}
        </section>
      )}

      {section === "settings" && (
        <section className="panel">
          <div className="setting-row">
            <div className="setting-copy">
              <strong>Appearance</strong>
              <span>Stored in this browser only.</span>
            </div>
            <div className="choice">
              <button className="btn btn-toggle" type="button" aria-pressed={theme === "light"} onClick={() => setTheme("light")}>☀️ Light</button>
              <button className="btn btn-toggle" type="button" aria-pressed={theme === "dark"} onClick={() => setTheme("dark")}>🌙 Dark</button>
            </div>
          </div>

          <div className="setting-row">
            <div className="setting-copy">
              <strong>Timestamps</strong>
              <span>
                Affects full timestamps shown on hover or tap. Camera capture times
                are always shown as the camera recorded them.
              </span>
            </div>
            <div className="choice">
              <button className="btn btn-toggle" type="button" aria-pressed={clock === "local"} onClick={() => setClock("local")}>Local</button>
              <button className="btn btn-toggle" type="button" aria-pressed={clock === "utc"} onClick={() => setClock("utc")}>UTC</button>
            </div>
          </div>

          <p style={{ marginTop: 16, color: "var(--muted)", fontSize: 13 }}>
            Right now: <TimeStamp value={new Date().toISOString()} />
          </p>
        </section>
      )}

      {section === "cli" && (
        <section className="panel">
          <h3>Bulk upload from the command line</h3>
          <p>Point this at a directory of images.</p>
          <pre className="cli-snippet"><code>{snippet}</code></pre>
          <button className="btn" type="button"
                  onClick={() => navigator.clipboard?.writeText(snippet)}>
            Copy snippet
          </button>
        </section>
      )}

      {section === "uploads" && (
        <section>
          {error && <div className="form-error">{error}</div>}
          {loadError && <LoadError error={loadError} retry={reload} />}
          {loading && <p className="empty">Loading uploads…</p>}
          {!loading && !loadError && images.length === 0 && <p className="empty">No uploads yet.</p>}
          <div className="feed">
            {images.map(image => (
              <PostCard key={image.id} post={image}>
                <button type="button" className="btn btn-danger delete-post"
                        disabled={deletingId === image.id}
                        onClick={() => deletePost(image)}>
                  {deletingId === image.id ? "Deleting…" : "Delete"}
                </button>
              </PostCard>
            ))}
          </div>
        </section>
      )}
    </div>
  );
}
