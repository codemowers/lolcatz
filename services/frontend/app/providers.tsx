"use client";
import { SessionProvider, signIn, useSession } from "next-auth/react";
import { AdminProvider } from "../lib/admin-access";
import { PreferencesProvider } from "../lib/preferences";

export function SessionGuard({ children }: { children: React.ReactNode }) {
  const { data: session, update } = useSession();
  if (!process.env.NEXT_PUBLIC_DEV_AUTH_TOKEN && session?.error === "RefreshTokenRetry" && !session.accessToken) {
    return <div className="panel auth-prompt" role="alert">
      <h3>Sign-in temporarily unavailable</h3>
      <p>Sign-in service temporarily unavailable. Your session will retry automatically.</p>
      <button className="btn" type="button" onClick={() => update()}>Retry</button>
    </div>;
  }
  if (!process.env.NEXT_PUBLIC_DEV_AUTH_TOKEN && session?.error === "RefreshTokenError") {
    return <div className="panel auth-prompt" role="alert">
      <h3>Sign in again</h3>
      <p>Your session could not be renewed. Sign in again to continue.</p>
      <button className="btn" type="button" onClick={() => signIn("passmower", { callbackUrl: window.location.href })}>Sign in again</button>
    </div>;
  }
  return <>{children}</>;
}

export function Providers({ children }: { children: React.ReactNode }) {
  return (
    <SessionProvider refetchInterval={60} refetchOnWindowFocus refetchWhenOffline={false}>
      <PreferencesProvider><AdminProvider>{children}</AdminProvider></PreferencesProvider>
    </SessionProvider>
  );
}
