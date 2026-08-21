// ── Token expiry handling ─────────────────────────────────────────────────────

// Tracks whichever element most recently triggered an htmx request, so a
// successful silent refresh (below) can replay that exact interaction rather
// than silently dropping it. Recorded via plain DOM events instead of any
// htmx-internal API/event-detail shape, so it keeps working across htmx
// version upgrades (this app vendors htmx 4.x, still in beta).
var lastHtmxTrigger = null;
document.addEventListener('submit', function (e) {
  if (e.target instanceof HTMLFormElement && (e.target.hasAttribute('hx-post') || e.target.hasAttribute('hx-get') || e.target.hasAttribute('hx-put') || e.target.hasAttribute('hx-patch') || e.target.hasAttribute('hx-delete'))) {
    lastHtmxTrigger = e.target;
  }
});
document.addEventListener('click', function (e) {
  var el = e.target.closest('[hx-get],[hx-post],[hx-put],[hx-patch],[hx-delete]');
  if (el && !(el instanceof HTMLFormElement)) {
    lastHtmxTrigger = el;
  }
});

// ── Prevent double-submission on plain (non-htmx) form posts ─────────────────

// htmx forms/buttons use hx-disable (see templates) to disable themselves for
// the duration of the request; that only applies to htmx-driven requests, so
// this is the equivalent safety net for a handful of plain <form method=post>
// submissions (e.g. sign-out) that don't go through htmx at all.
document.addEventListener('submit', function (e) {
  var form = e.target;
  if (!(form instanceof HTMLFormElement)) return;
  if (form.hasAttribute('hx-post') || form.hasAttribute('hx-get') || form.hasAttribute('hx-put') || form.hasAttribute('hx-patch') || form.hasAttribute('hx-delete')) return;
  if (form.method.toLowerCase() !== 'post') return;
  var btn = form.querySelector('button[type="submit"], input[type="submit"]');
  if (btn) btn.disabled = true;
});

// Firefox and Safari restore a bfcache page with the disabled state intact,
// so a user who presses Back would otherwise find the form unusable.
window.addEventListener('pageshow', function (e) {
  if (!e.persisted) return;
  document.querySelectorAll('form button[type="submit"][disabled], form input[type="submit"][disabled]').forEach(function (btn) {
    btn.disabled = false;
  });
});

// When the server fires the token-expired event, attempt a silent refresh.
// If it succeeds, replay whichever element triggered the request that failed
// — re-dispatching its natural default event (submit for forms, click for
// everything else) so htmx's own listener picks it up and reissues the
// request normally. If the refresh fails, send the user back to the
// dashboard (the site's sole sign-in entry point via Discord OAuth).
document.body.addEventListener('token-expired', async function () {
  try {
    var res = await fetch('/auth/refresh', { method: 'POST' });
    if (res.ok) {
      if (lastHtmxTrigger instanceof HTMLFormElement) {
        lastHtmxTrigger.requestSubmit();
      } else if (lastHtmxTrigger) {
        lastHtmxTrigger.click();
      }
    } else {
      window.location.href = '/dashboard';
    }
  } catch (_) {
    window.location.href = '/dashboard';
  }
});

// ── Landing page: "Get Started" opens a login modal ──────────────────────────

// Jumping straight to Discord's consent screen on the first click felt
// unexpected for a visitor who might just be looking around, so "Get
// Started" opens this confirmation modal instead of linking directly.
(function () {
  var modal = document.getElementById('get-started-modal');
  if (!modal) return;
  document.querySelectorAll('.js-get-started').forEach(function (btn) {
    btn.addEventListener('click', function () {
      modal.showModal();
    });
  });
})();

// ── Profile page: "Add a server" modal ────────────────────────────────────────

// The modal offers two paths — install the bot elsewhere, or register into a
// server it's already in — and picking one reveals just that sub-section,
// following the same show/hide-by-class idiom as the rest of this file.
(function () {
  var modal = document.getElementById('add-server-modal');
  if (!modal) return;

  document.querySelectorAll('.js-add-server-modal').forEach(function (btn) {
    btn.addEventListener('click', function () {
      modal.showModal();
    });
  });

  modal.querySelectorAll('.js-add-choice').forEach(function (btn) {
    btn.addEventListener('click', function () {
      modal.querySelectorAll('.js-add-section').forEach(function (section) {
        section.classList.add('hidden');
      });
      var target = document.getElementById(btn.dataset.target);
      if (target) target.classList.remove('hidden');
    });
  });
})();

// ── Guild dashboard: reporting channel picker + confirmation modal ──────────

function updateChannelPickerFilter(picker) {
  if (!picker) return;
  var input = picker.querySelector('.js-channel-picker-filter');
  var list = picker.querySelector('[data-channel-picker-list]');
  var rows = picker.querySelectorAll('.js-channel-picker-option-row');
  var empty = picker.querySelector('.js-channel-picker-empty');
  // Strip a leading "#" so re-filtering after a selection (which fills the
  // input with "#channel-name" as confirmation) still matches — row names
  // are stored without it.
  var filter = input ? input.value.toLowerCase().replace(/^#/, '') : '';
  var visibleCount = 0;

  rows.forEach(function (row) {
    var channelName = (row.dataset.channelName || '').toLowerCase();
    var matches = channelName.indexOf(filter) !== -1;
    row.classList.toggle('hidden', !matches);
    if (matches) visibleCount += 1;
  });

  // Stay collapsed until the user actually types something — an empty
  // filter matches every row, which would otherwise dump the full list open
  // as soon as the box gets focus.
  if (list) list.classList.toggle('hidden', filter === '' || visibleCount === 0);
  if (empty) empty.classList.toggle('hidden', filter === '' || visibleCount !== 0);
}

// The channel-region partial (including this picker, button, and modal) gets
// swapped in fresh via htmx after every save (hx-swap="outerHTML"), so these
// listeners use `document` delegation instead of binding directly — a listener
// attached once at page load wouldn't survive the swap.
document.addEventListener('input', function (e) {
  var input = e.target.closest('.js-channel-picker-filter');
  if (!input) return;
  updateChannelPickerFilter(input.closest('.js-channel-picker'));
});

// The list is a plain in-flow element, shown/hidden by toggling `hidden`
// directly (not a daisyUI `dropdown`/`dropdown-content` overlay) — an
// absolutely-positioned overlay here ended up covering the permission
// warning and Confirm/Cancel buttons underneath it whenever it was open,
// since it doesn't push flow content down. It only opens once the user
// types (see updateChannelPickerFilter) — focusing the box alone shouldn't
// dump the full channel list onto the page. Selecting all text on focus
// means the first keystroke replaces it rather than appending to whatever
// was left from a previous pick. `focus` doesn't bubble, so this has to run
// in the capture phase to work with `document`-level delegation.
document.addEventListener('focus', function (e) {
  var input = e.target.closest && e.target.closest('.js-channel-picker-filter');
  if (!input) return;
  input.select();
}, true);

// Clicking anywhere outside an open picker closes its list.
document.addEventListener('click', function (e) {
  document.querySelectorAll('.js-channel-picker').forEach(function (picker) {
    if (picker.contains(e.target)) return;
    var list = picker.querySelector('[data-channel-picker-list]');
    if (list) list.classList.add('hidden');
  });
});

document.addEventListener('click', function (e) {
  var option = e.target.closest('.js-channel-picker-option');
  if (!option) return;
  var picker = option.closest('.js-channel-picker');
  var form = option.closest('form');
  var channelInput = form ? form.querySelector('input[name="channel_id"]') : null;
  var channelName = option.dataset.channelName || 'the selected channel';

  if (channelInput) {
    channelInput.value = option.dataset.channelId || '';
    channelInput.dataset.channelName = channelName;
  }
  if (picker) {
    picker.querySelectorAll('.js-channel-picker-option').forEach(function (btn) {
      var selected = btn === option;
      btn.classList.toggle('active', selected);
      btn.setAttribute('aria-selected', selected ? 'true' : 'false');
    });
    var filterInput = picker.querySelector('.js-channel-picker-filter');
    if (filterInput) filterInput.value = channelName;
    var list = picker.querySelector('[data-channel-picker-list]');
    if (list) list.classList.add('hidden');
  }
  if (document.activeElement) document.activeElement.blur();
});

document.addEventListener('click', function (e) {
  var btn = e.target.closest('.js-open-channel-confirm');
  if (!btn) return;
  var modal = document.getElementById('channel-confirm-modal');
  if (modal) modal.showModal();
});

document.addEventListener('click', function (e) {
  var btn = e.target.closest('.js-open-channel-remove-confirm');
  if (!btn) return;
  var modal = document.getElementById('channel-remove-modal');
  if (modal) modal.showModal();
});
