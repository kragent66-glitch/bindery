import { ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router'
import { useTranslation } from 'react-i18next'
import { useAuth } from './AuthContext'

// AuthGuard wraps the main app. Decision tree:
//
//   loading → render a quiet placeholder
//   no status because the check failed → offer a retry, not the login page
//   setup required → force /setup
//   not authenticated → force /login
//   authenticated → render children
//
// /login and /setup render outside of the guard (they're routed above it),
// so they never get bounced by their own redirects.
export default function AuthGuard({ children }: { children: ReactNode }) {
  const { status, loading, statusError, refresh } = useAuth()
  const { t } = useTranslation()
  const location = useLocation()

  if (loading) {
    return (
      <div className="min-h-screen flex items-center justify-center text-slate-500 dark:text-zinc-500 text-sm">
        Loading…
      </div>
    )
  }

  // The status check failed before any status loaded. That says nothing
  // about the session, so do not send the user to sign in again.
  if (!status && statusError) {
    return (
      <div className="min-h-screen flex flex-col items-center justify-center gap-3 px-4 text-center text-slate-500 dark:text-zinc-500 text-sm">
        <p>{t('common.serverUnavailable', 'Bindery is not answering right now.')}</p>
        <button
          type="button"
          onClick={() => { void refresh() }}
          className="rounded px-3 py-1.5 border border-slate-300 dark:border-zinc-700 text-slate-700 dark:text-zinc-300"
        >
          {t('common.retry', 'Retry')}
        </button>
      </div>
    )
  }
  if (status?.setupRequired && location.pathname !== '/setup') {
    return <Navigate to="/setup" replace />
  }
  if (!status?.authenticated && !status?.setupRequired && location.pathname !== '/login') {
    return <Navigate to="/login" replace />
  }

  return <>{children}</>
}
