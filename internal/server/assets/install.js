// The front door's behaviour: fill it from this repository, then offer.
//
// Chrome will not install anything without a person asking, and will only let a
// page ask during a real click. So the install half is: wait for Chrome to say
// the offer is live, enable the one button, and then get out of the way.
//
// This page never decides whether plum is installed — the head of install.html
// does that, from the display mode, before anything paints. This file only
// handles the case where it is not.
(function () {
  const $ = (id) => document.getElementById(id);
  let offer = null;

  const note = (text, warn) => {
    const n = $('note');
    n.textContent = text;
    n.classList.toggle('warn', !!warn);
  };

  $('where-host').textContent = location.host;

  // --- the repository's own numbers -------------------------------------
  // Every figure on this page is fetched rather than written, so the page can
  // never promise a count the window would then contradict. If the server has
  // nothing to say, the page says that, in its own words.

  const get = (path) => fetch(path).then((r) => (r.ok ? r.json() : null)).catch(() => null);

  Promise.all([get('/api/health'), get('/api/debt'), get('/api/tests')]).then(([health, debt, tests]) => {
    if (health && health.repo) {
      const name = health.repo.split('/').filter(Boolean).pop() || health.repo;
      // "plum · plum" when the repository is plum itself says nothing twice.
      if (name !== 'plum') $('repo').textContent = name;
    }
    const n = tests && Array.isArray(tests.tests) ? tests.tests.length : 0;
    if (n) $('bar-n').textContent = n + ' tests';

    if (!debt || !debt.total) {
      // Nothing recorded yet. Honest, and still the window's job to show.
      $('n').textContent = '0';
      $('head-rest').textContent = 'to read — plum starts counting with the next session.';
      $('count').textContent = '0';
      $('of').textContent = 'nothing recorded yet';
      $('specimen').querySelector('.frame').classList.add('clear');
      $('what').classList.add('clear');
      const li = document.createElement('li');
      li.className = 'none';
      li.textContent = 'Run your agent under plum and this fills with what changed.';
      $('owed').appendChild(li);
      return;
    }

    const unmet = debt.unmet | 0, total = debt.total | 0;
    $('n').textContent = String(unmet);
    $('head-rest').textContent = unmet === 1
      ? 'piece of this codebase was changed by an agent and you have not read it yet.'
      : 'pieces of this codebase were changed by an agent and you have not read them yet.';
    if (!unmet) {
      $('head-rest').textContent = 'left to read. You have met everything the agent changed here.';
      $('what').classList.add('clear');
      $('specimen').querySelector('.frame').classList.add('clear');
    }

    $('count').textContent = String(unmet);
    $('of').textContent = 'of ' + total + ' changed, not yet read' + (health && health.session ? ' · ' + health.session : '');
    // Next frame, so the bar is seen to fill rather than appear filled.
    requestAnimationFrame(() => {
      $('meter').style.width = Math.round(100 * (total - unmet) / total) + '%';
    });

    const list = (debt.worklist || []).slice(0, 3);
    for (const item of list) {
      const li = document.createElement('li');
      const left = document.createElement('span');
      const sym = document.createElement('span'); sym.className = 'sym'; sym.textContent = item.name || item.symbol;
      const file = document.createElement('span'); file.className = 'file'; file.textContent = item.file || '';
      left.append(sym, file);
      const why = document.createElement('span'); why.className = 'why'; why.textContent = item.why || '';
      li.append(left, why);
      $('owed').appendChild(li);
    }
    const rest = (debt.worklist || []).length - list.length;
    if (rest > 0) $('more').textContent = 'and ' + rest + ' more, in the order worth reading them';
  });

  // The prompt for the agent, copied whole. A selection across a wrapped code
  // block is fiddly; a button is not.
  $('copy').onclick = async () => {
    try {
      await navigator.clipboard.writeText($('ask-text').textContent);
      $('copy').textContent = 'copied';
      $('copy').classList.add('did');
      setTimeout(() => { $('copy').textContent = 'copy'; $('copy').classList.remove('did'); }, 1600);
    } catch (_) {
      $('copy').textContent = 'select it';
    }
  };

  // --- the offer ----------------------------------------------------------

  // Chrome fires this in place of showing its own install UI. It is the only
  // handle on the prompt there is, and it arrives a beat after load rather than
  // with it — hence starting disabled and saying so.
  addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault();
    offer = e;
    $('go').disabled = false;
    note('Chrome will ask you to confirm');
  });

  // Installed from somewhere other than this button — the browser's own menu,
  // or another window on the same repository.
  addEventListener('appinstalled', done);

  $('go').onclick = async () => {
    if (!offer) return;
    $('go').disabled = true;
    note('Waiting for you to confirm…');
    offer.prompt();
    const choice = await offer.userChoice;
    // Chrome will not re-offer the same page in the same session, so a dismissal
    // has to be recoverable by hand rather than by clicking again.
    offer = null;
    if (choice.outcome === 'accepted') {
      done();
      return;
    }
    note('Not installed. Reload this window to be asked again.', true);
  };

  function done() {
    offer = null;
    $('gate').classList.add('done');
    $('head-rest').textContent = 'plum is installed.';
    // Chrome opens the installed app the moment it is installed, so by the time
    // this is read the real window already exists. This one is now the spare.
    $('lede').textContent =
      'It opened in its own window, with its own controls — look for the plum ' +
      'icon in the dock. Everything from here on happens there, and this window ' +
      'can be closed. The rest of this page stays as a reference.';
    note('installed');
  }

  // A browser that cannot install web apps never fires the offer, and would
  // otherwise leave the reader on a page whose only button never lights up.
  // No offer within a couple of seconds means one of two things, and the page
  // cannot tell them apart from here: plum is already installed (Chrome does not
  // offer to install twice), or this browser cannot install applications at all.
  // So it says both, and points at the way out of the common one — the app is
  // already there, in the dock.
  setTimeout(() => {
    if (offer || $('gate').classList.contains('done')) return;
    // The button cannot launch a desktop app from here, so it stops being an
    // install button and becomes the instruction for the common case.
    $('go').disabled = true;
    note('already installed? open plum from your dock. otherwise this browser cannot install apps — Chrome, Edge or Brave can', true);
  }, 2500);

  // The way past, for that browser and for anyone who would rather not install.
  // Deliberately per-window and not remembered: it is a detour, not a setting,
  // and there is nothing in the bar to switch it back off with.
  $('anyway').onclick = (e) => {
    e.preventDefault();
    try { sessionStorage.setItem('plum-unframed', '1'); } catch (_) {}
    location.replace('/probe.html');
  };
})();
