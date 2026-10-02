/* exe-stats: counts a page view of the page this script is on. No cookie,
   nothing stored in the browser, nothing sent but the page's address and
   where the reader came from. https://github.com/livid/exe-stats */
(function () {
  var tag = document.currentScript, id = tag && tag.getAttribute("data-site");
  if (!id) return;
  var to = new URL(tag.src).origin + "/e", last = "";
  function page() { return location.href.split("#")[0]; }
  function send() {
    var u = page();
    if (u === last) return; // a hash change, or the same page pushed again
    // the first view came from wherever the reader did; a later one, in
    // a page that changes its own address, came from the page before
    var body = JSON.stringify({ s: id, u: u, r: last || document.referrer });
    last = u;
    try {
      if (navigator.sendBeacon && navigator.sendBeacon(to, body)) return;
      fetch(to, { method: "POST", body: body, keepalive: true, mode: "no-cors" }).catch(function () {});
    } catch (e) {}
  }
  function start() {
    var push = history.pushState;
    if (push) history.pushState = function () { push.apply(this, arguments); send(); };
    addEventListener("popstate", send);
    send();
  }
  // a page the browser renders ahead of a visit is not a view until it is shown
  if (document.prerendering) document.addEventListener("prerenderingchange", start, { once: true });
  else start();
})();
