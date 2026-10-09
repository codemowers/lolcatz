"use client";

import Link from "next/link";
import { useId } from "react";
import { useAdminAccess } from "../lib/admin-access";

export function AdminLink() {
  const { status } = useAdminAccess();
  const tooltipId = useId();
  if (status === "denied" || status === "signed-out") return null;
  if (status === "allowed") {
    return <Link className="btn btn-ghost" href="/profile/boards">Manage boards</Link>;
  }
  const reason = {
    checking: "Checking board administration availability…",
    unavailable: "Board administration is currently unavailable.",
    expired: "Your session has expired. Sign in again to manage boards.",
  }[status];
  return <span className="admin-action" tabIndex={0} aria-label="Manage boards" aria-describedby={tooltipId}>
    <button className="btn btn-ghost" type="button" disabled>Manage boards</button>
    <span className="admin-action-tooltip" id={tooltipId} role="tooltip">{reason}</span>
  </span>;
}
