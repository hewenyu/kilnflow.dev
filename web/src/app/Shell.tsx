import {
  CodeReviewIcon,
  CopilotIcon,
  CpuIcon,
  GearIcon,
  GitMergeQueueIcon,
  GlobeIcon,
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
import { Msg, Trans, useLocale, useT } from "../i18n/locale";
import { localeNames, locales, type MsgKey } from "../i18n/messages";
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

const tabs: readonly {
  nav: string;
  href: string;
  label: MsgKey;
  icon: typeof HomeIcon;
  admin: boolean;
}[] = [
  { nav: "overview", href: "/", label: "nav.overview", icon: HomeIcon, admin: false },
  { nav: "runs", href: "/runs", label: "nav.runs", icon: PlayIcon, admin: false },
  {
    nav: "merge_queue",
    href: "/merge-queue",
    label: "nav.mergeQueue",
    icon: GitMergeQueueIcon,
    admin: false,
  },
  {
    nav: "ai_requests",
    href: "/ai/requests",
    label: "nav.aiRequests",
    icon: CodeReviewIcon,
    admin: false,
  },
  {
    nav: "ai_dev",
    href: "/ai-dev",
    label: "nav.aiDev",
    icon: CopilotIcon,
    admin: false,
  },
  {
    nav: "runners",
    href: "/runners",
    label: "nav.runners",
    icon: ServerIcon,
    admin: false,
  },
  {
    nav: "ai_runners",
    href: "/ai/runners",
    label: "nav.aiRunners",
    icon: CpuIcon,
    admin: false,
  },
  {
    nav: "settings",
    href: "/settings",
    label: "nav.settings",
    icon: GearIcon,
    admin: true,
  },
];

/** UserMenu 是身份菜单。退出登录提交 POST 表单（带 csrf），其他网站不能让用户被动退出。 */
function UserMenu({ session, csrf }: { session: Session; csrf: string }) {
  const logout = useRef<HTMLFormElement>(null);
  const t = useT();
  const { locale, setLocale } = useLocale();
  const role = t(session.Admin ? "role.admin" : "role.member");
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
            aria-label={t("shell.userMenu", { login: session.Login, role })}
            variant="invisible"
          />
        </ActionMenu.Anchor>
        <ActionMenu.Overlay align="end" width="small">
          <span className={css.menuWho}>
            <strong>{session.Login}</strong>
            {role}
            {session.ViaToken ? t("shell.viaToken") : ""}
          </span>
          <ActionList>
            <ActionList.Divider />
            {session.Admin && (
              <ActionList.LinkItem href="/settings/account">
                {t("shell.account")}
              </ActionList.LinkItem>
            )}
            <ActionList.Group selectionVariant="single">
              <ActionList.GroupHeading>{t("locale.label")}</ActionList.GroupHeading>
              {locales.map((l) => (
                <ActionList.Item
                  key={l}
                  lang={l}
                  selected={l === locale}
                  onSelect={() => setLocale(l)}
                >
                  {localeNames[l]}
                </ActionList.Item>
              ))}
            </ActionList.Group>
            <ActionList.Divider />
            <ActionList.Item onSelect={() => logout.current?.submit()}>
              <ActionList.LeadingVisual>
                <SignOutIcon />
              </ActionList.LeadingVisual>
              {t("shell.logout")}
            </ActionList.Item>
          </ActionList>
        </ActionMenu.Overlay>
      </ActionMenu>
    </>
  );
}

/** LocaleSwitch 是未登录页面（登录页、错误页）右上角的语言切换。 */
function LocaleSwitch() {
  const t = useT();
  const { locale, setLocale } = useLocale();
  return (
    <div className={css.localeSwitch}>
      <ActionMenu>
        <ActionMenu.Button
          leadingVisual={GlobeIcon}
          size="small"
          variant="invisible"
          aria-label={t("locale.switch", { current: localeNames[locale] })}
        >
          {localeNames[locale]}
        </ActionMenu.Button>
        <ActionMenu.Overlay align="end" width="small">
          <ActionList selectionVariant="single" aria-label={t("locale.label")}>
            {locales.map((l) => (
              <ActionList.Item
                key={l}
                lang={l}
                selected={l === locale}
                onSelect={() => setLocale(l)}
              >
                {localeNames[l]}
              </ActionList.Item>
            ))}
          </ActionList>
        </ActionMenu.Overlay>
      </ActionMenu>
    </div>
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
  const t = useT();
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
            aria-label={t("shell.dismiss")}
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
      {session.Bound ? (
        <Trans
          id="shell.tokenNoticeBound"
          tags={{
            link: (c) => (
              <Link href="/auth/github?next=%2Fsettings%2Faccount">{c}</Link>
            ),
          }}
        />
      ) : (
        <Trans
          id="shell.tokenNoticeUnbound"
          tags={{ link: (c) => <Link href="/settings/account">{c}</Link> }}
        />
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
  const t = useT();
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
        {t("shell.skip")}
      </a>
      {loading && (
        <div
          className={css.progress}
          role="progressbar"
          aria-label={t("shell.loading")}
        />
      )}
      {!bare && (
        <header className={css.header}>
          <div className={css.top}>
            <a className={css.brand} href="/" aria-label={t("shell.home")}>
              <KilnMark />
              <span>Kiln</span>
            </a>
            <span className={css.spacer} />
            <UserMenu session={session} csrf={env.CSRF} />
          </div>
          <div className={css.nav}>
            <UnderlineNav aria-label={t("nav.main")}>
              {tabs
                .filter((tab) => !tab.admin || session.Admin)
                .map((tab) => (
                  <UnderlineNav.Item
                    key={tab.nav}
                    href={tab.href}
                    leadingVisual={<tab.icon />}
                    aria-current={env.Nav === tab.nav ? "page" : undefined}
                    counter={
                      tab.nav === "runs" && env.Active > 0
                        ? env.Active
                        : undefined
                    }
                  >
                    {t(tab.label)}
                  </UnderlineNav.Item>
                ))}
            </UnderlineNav>
          </div>
        </header>
      )}
      {session && !bare && <TokenNotice session={session} />}
      {bare && <LocaleSwitch />}
      {expired && (
        <Notice tone="warn">
          <Trans
            id="shell.expired"
            tags={{ link: (c) => <Link href={loginURL(here)}>{c}</Link> }}
          />
        </Notice>
      )}
      {failure !== null && (
        <Notice tone="error" onDismiss={dismissFailure}>
          <Msg text={failure} />
        </Notice>
      )}
      {view?.flash != null && (
        <Notice tone="info" onDismiss={dismissFlash}>
          <Msg text={view.flash} />
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
