import { fireEvent, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import errorFixture from "../../dev/fixtures/error.json";
import { envelope, renderPage } from "../test/render";

// 外壳：顶栏、标签导航、身份菜单、admin token 提示和跳转后的提示。

describe("外壳", () => {
  it('管理员看到全部标签，当前标签标为 aria-current，"运行"旁显示进行中的数量', () => {
    renderPage(envelope(errorFixture, { Nav: "runs", Active: 3 }), "/runs/9");
    const nav = screen.getByRole("navigation", { name: "主导航" });
    const links = within(nav).getAllByRole("link");
    expect(links.map((l) => l.getAttribute("href"))).toEqual([
      "/",
      "/runs",
      "/merge-queue",
      "/ai/requests",
      "/ai-dev",
      "/runners",
      "/ai/runners",
      "/settings",
    ]);
    expect(within(nav).getByRole("link", { name: /运行/ })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(within(nav).getByRole("link", { name: /运行/ })).toHaveTextContent(
      "3",
    );
    expect(screen.getByRole("link", { name: "跳到正文" })).toHaveAttribute(
      "href",
      "#main",
    );
  });

  it('成员看不到"设置"；身份菜单显示角色和退出', async () => {
    renderPage(
      envelope(errorFixture, {
        Session: { Login: "bob", Admin: false, ViaToken: false, Bound: false },
        CSRF: "tok",
      }),
      "/",
    );
    expect(
      within(screen.getByRole("navigation", { name: "主导航" })).queryByRole(
        "link",
        { name: /设置/ },
      ),
    ).not.toBeInTheDocument();
    await userEvent.click(
      screen.getByRole("button", { name: "bob（成员）的菜单" }),
    );
    expect(await screen.findByText("成员")).toBeInTheDocument();
    // 退出登录提交 POST 表单（带 csrf），不是链接
    const submit = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(function (this: HTMLFormElement) {
        expect(this.method).toBe("post");
        expect(this.getAttribute("action")).toBe("/logout");
        expect(new FormData(this).get("csrf")).toBe("tok");
      });
    await userEvent.click(screen.getByRole("menuitem", { name: /退出登录/ }));
    expect(submit).toHaveBeenCalledOnce();
    submit.mockRestore();
    expect(
      screen.queryByRole("menuitem", { name: "登录与身份" }),
    ).not.toBeInTheDocument();
  });

  it("用 admin token 登录时提示绑定或改用 GitHub 登录", () => {
    renderPage(
      envelope(errorFixture, {
        Session: { Login: "admin", Admin: true, ViaToken: true, Bound: false },
      }),
      "/",
    );
    expect(screen.getByText(/当前用 admin token 登录/)).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "绑定你的 GitHub 账号" }),
    ).toHaveAttribute("href", "/settings/account");
  });

  it("跳转后的提示用 role=status 读出，可以关闭", async () => {
    renderPage(envelope(errorFixture, { Flash: "已经取消 run #7" }), "/runs/7");
    const flash = screen.getByRole("status");
    expect(flash).toHaveTextContent("已经取消 run #7");
    await userEvent.click(
      within(flash).getByRole("button", { name: "关闭提示" }),
    );
    expect(screen.queryByText("已经取消 run #7")).not.toBeInTheDocument();
  });

  it("页面更新时身份菜单的头像不重新挂载：加载失败后保持首字母，不反复请求图片", async () => {
    renderPage(
      envelope(errorFixture, {
        Session: { Login: "bob", Admin: false, ViaToken: false, Bound: false },
        Flash: "已保存",
      }),
      "/",
    );
    const menu = screen.getByRole("button", { name: "bob（成员）的菜单" });
    const img = menu.querySelector("img");
    expect(img).not.toBeNull();
    if (img) fireEvent.error(img);
    expect(menu.querySelector("img")).toBeNull();
    // 关闭提示会更新当前页面，外壳重新渲染
    await userEvent.click(screen.getByRole("button", { name: "关闭提示" }));
    expect(screen.queryByText("已保存")).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "bob（成员）的菜单" }).querySelector("img"),
    ).toBeNull();
  });

  it("未登录时不显示顶栏", () => {
    renderPage(envelope(errorFixture, { Session: null, CSRF: "" }), "/nope");
    expect(
      screen.queryByRole("navigation", { name: "主导航" }),
    ).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Kiln/ })).toHaveAttribute(
      "href",
      "/",
    );
  });
});
