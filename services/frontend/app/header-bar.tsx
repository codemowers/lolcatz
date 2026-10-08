"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { usePathname, useRouter } from "next/navigation";
import { signIn, useSession } from "next-auth/react";
import { requestJSON, isRequestError } from "../lib/api";
import type { Board } from "../lib/boards";

export function HeaderBar() {
  const pathname = usePathname();
  const router = useRouter();
  const { data: session, status } = useSession();
  const devToken = process.env.NEXT_PUBLIC_DEV_AUTH_TOKEN;
  const [query, setQuery] = useState("");
  const [boards, setBoards] = useState<Board[]>([]);

  useEffect(() => {
    const refresh = () => requestJSON<Board[]>("/api/browse/boards")
      .then(data => setBoards(Array.isArray(data) ? data.slice(0, 3) : []))
      .catch(error => { if (!isRequestError(error)) throw error; });
    refresh();
    window.addEventListener("boards-changed", refresh);
    return () => window.removeEventListener("boards-changed", refresh);
  }, [pathname]);

  useEffect(() => { setQuery(new URLSearchParams(window.location.search).get("q") || ""); }, [pathname]);

  const submitSearch = (event: React.FormEvent) => {
    event.preventDefault();
    const trimmed = query.trim();
    if (trimmed) router.push(`/search?q=${encodeURIComponent(trimmed)}`);
  };

  // A failed refresh still reports "authenticated"; offer sign-in instead.
  const authenticated = Boolean(devToken) || (status === "authenticated" && session?.error !== "RefreshTokenError");
  const label = devToken
    ? "developer@localhost"
    : session?.user?.name ?? session?.user?.email ?? "Profile";

  return (
    <header>
      <Link className="brand" href="/">
        <span className="brand-mark" aria-hidden="true">🐱</span>
        Can I Haz Kubernetes
      </Link>

      <nav>
        {boards.map(board => {
          const href = `/${board.id}`;
          const active = pathname === href;
          return (
            <Link key={board.id} href={href} className={active ? "active" : undefined}
               aria-current={active ? "page" : undefined} title={board.name}>
              /{board.id}/
            </Link>
          );
        })}
      </nav>

      {pathname !== "/search" && <form className="navbar-search" role="search" onSubmit={submitSearch}>
        <input
          type="search"
          value={query}
          onChange={event => setQuery(event.target.value)}
          placeholder="Search captions and tags…"
          aria-label="Search"
        />
      </form>}
      {/* Narrow viewports drop the inline field and link out instead. */}
      {pathname !== "/search" && <Link className="btn btn-ghost search-emoji" href="/search" aria-label="Search">🔍</Link>}

      {authenticated ? (
        <span className="auth-status">
          <Link className="btn btn-ghost" href="/profile/boards">Manage boards</Link>
          <Link href="/profile" className="profile-link">
            {session?.user?.image && <img src={session.user.image} alt="" />}
            <span>{label}</span>
          </Link>
        </span>
      ) : status === "loading" ? (
        <span className="auth-status">…</span>
      ) : (
        <button className="btn" type="button"
                onClick={() => signIn("passmower", { callbackUrl: window.location.href })}>
          Sign in
        </button>
      )}
    </header>
  );
}
