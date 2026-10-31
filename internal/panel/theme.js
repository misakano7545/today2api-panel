// 首屏主题预热：在样式表解析前决定明暗，避免刷新时闪一下夜间模式。
(function () {
  try {
    var saved = localStorage.getItem("t2a-theme");
    var theme = saved || (window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
    document.documentElement.setAttribute("data-theme", theme);
  } catch (e) {
    /* localStorage 不可用时保留 html 上的 dark 兜底 */
  }
})();
