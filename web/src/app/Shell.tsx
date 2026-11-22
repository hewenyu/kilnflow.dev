import {
  CodeReviewIcon,
  CopilotIcon,
  CpuIcon,
  GearIcon,
  GitMergeQueueIcon,
  HomeIcon,
  PlayIcon,
  ServerIcon,
  SignOutIcon,
  XIcon,
} from "@primer/octicons-react";
import {
  ActionList,
  ActionMenu,
  IconButton,
  Link,
  UnderlineNav,
} from "@primer/react";
import { type ReactNode, useRef } from "react";
import { Avatar } from "../components/Avatar";
import { loginURL } from "../lib/api";
import type { Envelope, Session } from "../lib/envelope";
import { useRouter } from "./router";
import css from "./Shell.module.css";

/** KilnMark 是 Kiln 的窑形标识。 */
export function KilnMark({ size = 32 }: { size?: number }) {
  return (
    <svg
      className={css.mark}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <path d="M4 21V11a8 8 0 0 1 16 0v10z" />
      <path d="M2 21h20" />
      <path d="M12 17.5c-1.6 0-2.5-1-2.5-2.3 0-1.6 1.6-2.4 2.5-4.2.9 1.8 2.5 2.6 2.5 4.2 0 1.3-.9 2.3-2.5 2.3z" />
    </svg>
  );
}

const tabs = [
  { nav: "overview", href: "/", label: "总览", icon: HomeIcon, admin: false },
  { nav: "runs", href: "/runs", label: "运行", icon: PlayIcon, admin: false },
  {
    nav: "merge_queue",
    href: "/merge-queue",
    label: "合并队列",
    icon: GitMergeQueueIcon,
    admin: false,
  },
  {
    nav: "ai_requests",
    href: "/ai/requests",
    label: "AI 请求",
    icon: CodeReviewIcon,
    admin: false,
  },
  {
    nav: "ai_dev",
    href: "/ai-dev",
    label: "AI 开发",
    icon: CopilotIcon,
    admin: false,
  },
  {
    nav: "runners",
    href: "/runners",
    label: "Runner",
    icon: ServerIcon,
    admin: false,
  },
  {
    nav: "ai_runners",
    href: "/ai/runners",
    label: "AI 执行节点",
    icon: CpuIcon,
    admin: false,
  },
  {
    nav: "settings",
    href: "/settings",
    label: "设置",
    icon: GearIcon,
    admin: true,
  },
] as const;

/** UserMenu 是身份菜单。退出登录提交 POST 表单（带 csrf），其他网站不能让用户被动退出。 */
function UserMenu({ session, csrf }: { session: Session; csrf: string }) {
  const logout = useRef<HTMLFormElement>(null);
  return (
    <>
      <form ref={logout} method="post" action="/logout" hidden>
        <input type="hidden" name="csrf" value={csrf} />
      </form>
      <ActionMenu>
        <ActionMenu.Anchor>
          <IconButton
            // 传元素而不是组件：每次渲染新建的组件会让头像在每次定时刷新时重新挂载（重新请求图片、加载失败的状态丢失）
            icon={<Avatar login={session.Login} size={24} />}
            aria-label={`${session.Login}（${session.Admin ? "管理员" : "成员"}）的菜单`}
            variant="invisible"
          />
        </ActionMenu.Anchor>
        <ActionMenu.Overlay align="end" width="small">
          <span className={css.menuWho}>
            <strong>{session.Login}</strong>
            {session.Admin ? "管理员" : "成员"}
            {session.ViaToken ? " · 用 admin token 登录" : ""}
          </span>
          <ActionList>
            <ActionList.Divider />
            {session.Admin && (
              <ActionList.LinkItem href="/settings/account">
                登录与身份
              </ActionList.LinkItem>
            )}
            <ActionList.Item onSelect={() => logout.current?.submit()}>
              <ActionList.LeadingVisual>
                <SignOutIcon />
              </ActionList.LeadingVisual>
              退出登录
            </ActionList.Item>
          </ActionList>
        </ActionMenu.Overlay>
      </ActionMenu>
    </>
  );
}

/** Notice 是顶栏下方的通知条。 */
function Notice({
  tone,
  children,
  onDismiss,
}: {
  tone: "info" | "warn" | "error";
  children: ReactNode;
  onDismiss?: () => void;
}) {
  return (
    <div
      className={`${css.notice} ${css[tone]}`}
      role={tone === "error" ? "alert" : "status"}
    >
      <div className={css.noticeInner}>
        <div className={css.noticeText}>{children}</div>
        {onDismiss && (
          <IconButton
            icon={XIcon}
            aria-label="关闭提示"
            variant="invisible"
            size="small"
            onClick={onDismiss}
          />
        )}
      </div>
    </div>
  );
}

/** TokenNotice 提醒用 admin token 登录的管理员改用 GitHub 登录（PLAN #16.5）。 */
function TokenNotice({ session }: { session: Session }) {
  if (!session.ViaToken) return null;
  return (
    <Notice tone="warn">
      当前用 admin token 登录。
      {session.Bound ? (
        <Link href="/auth/github?next=%2Fsettings%2Faccount">
          改用 GitHub 登录
        </Link>
      ) : (
        <>
          <Link href="/settings/account">绑定你的 GitHub 账号</Link>，之后用
          GitHub 登录。
        </>
      )}
    </Notice>
  );
}

/** Shell 是已登录页面的外壳：顶栏、标签导航、通知和内容区。 */
export function Shell({
  env,
  children,
}: {
  env: Envelope;
  children: ReactNode;
}) {
  const { view, failure, loading, expired, dismissFlash, dismissFailure } =
    useRouter();
  const session = env.Session;
  const here = view?.url ?? "/";
  const bare = !session || env.Nav === "login";

  return (
    <>
      <a
        className={css.skip}
        href="#main"
        onClick={(e) => {
          e.preventDefault();
          document.getElementById("main")?.focus();
        }}
      >
        跳到正文
      </a>
      {loading && (
        <div
          className={css.progress}
          role="progressbar"
          aria-label="正在加载页面"
        />
      )}
      {!bare && (
        <header className={css.header}>
          <div className={css.top}>
            <a className={css.brand} href="/" aria-label="Kiln 总览">
              <KilnMark />
              <span>Kiln</span>
            </a>
            <span className={css.spacer} />
            <UserMenu session={session} csrf={env.CSRF} />
          </div>
          <div className={css.nav}>
            <UnderlineNav aria-label="主导航">
              {tabs
                .filter((t) => !t.admin || session.Admin)
                .map((t) => (
                  <UnderlineNav.Item
                    key={t.nav}
                    href={t.href}
                    leadingVisual={<t.icon />}
                    aria-current={env.Nav === t.nav ? "page" : undefined}
                    counter={
                      t.nav === "runs" && env.Active > 0
                        ? env.Active
                        : undefined
                    }
                  >
                    {t.label}
                  </UnderlineNav.Item>
                ))}
            </UnderlineNav>
          </div>
        </header>
      )}
      {session && !bare && <TokenNotice session={session} />}
      {expired && (
        <Notice tone="warn">
          登录已过期，自动更新已停止。
          <Link href={loginURL(here)}>重新登录</Link>
        </Notice>
      )}
      {failure && (
        <Notice tone="error" onDismiss={dismissFailure}>
          {failure}
        </Notice>
      )}
      {view?.flash && (
        <Notice tone="info" onDismiss={dismissFlash}>
          {view.flash}
        </Notice>
      )}
      <main
        id="main"
        tabIndex={-1}
        className={env.Page === "login" ? css.bare : css.main}
      >
        {children}
      </main>
    </>
  );
}
