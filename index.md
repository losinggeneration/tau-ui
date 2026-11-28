---
title: Tau UI
---

<style>
  .shot img {
    display: block;
    margin: 0 auto;
    width: 100%;
    max-width: 480px;
    cursor: zoom-in;
  }

  #lightbox {
    position: fixed;
    inset: 0;
    z-index: 999;
    background: rgba(0, 0, 0, 0.88);
    display: flex;
    align-items: center;
    justify-content: center;
    cursor: zoom-out;
  }

  #lightbox[hidden] { display: none; }

  #lightbox img {
    max-width: calc(100vw - 48px);
    max-height: calc(100vh - 48px);
  }
</style>

<figure class="shot">
  <a id="screenshot" href="tau-ui.png" target="_blank" rel="noopener">
    <img src="tau-ui.png" alt="Tau UI — the instance sidebar on the left, an active conversation on the right">
  </a>
</figure>

<div id="lightbox" hidden>
  <img src="tau-ui.png" alt="">
</div>

<script>
  (function () {
    var lightbox = document.getElementById('lightbox');
    document.getElementById('screenshot').addEventListener('click', function (e) {
      e.preventDefault();
      lightbox.hidden = false;
    });
    lightbox.addEventListener('click', function () {
      lightbox.hidden = true;
    });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') lightbox.hidden = true;
    });
  })();
</script>

{% include_relative README.md %}
