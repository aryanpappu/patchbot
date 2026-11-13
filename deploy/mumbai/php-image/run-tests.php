<?php
// patchbot test entrypoint (runs inside the sandbox, no network):
// copy pre-installed vendor, build the autoloader, stub the Vite manifest, run phpunit.
$work = getcwd();
if (!file_exists("$work/vendor")) {
    passthru('cp -R /opt/app/vendor ' . escapeshellarg("$work/vendor"), $cp);
    if ($cp !== 0) { exit($cp); }
}
putenv('COMPOSER_HOME=/tmp/composer');
putenv('APP_KEY=base64:' . base64_encode(random_bytes(32)));
foreach (['bootstrap/cache', 'storage/framework/cache', 'storage/framework/views', 'storage/framework/sessions', 'storage/logs'] as $d) {
    @mkdir("$work/$d", 0777, true);
}
// Tests render Blade views with @vite; the real frontend build is not available
// offline, so map every resources/ asset to a placeholder entry.
$manifest = "$work/public/build/manifest.json";
if (!file_exists($manifest) && is_dir("$work/resources")) {
    $entries = [];
    $it = new RecursiveIteratorIterator(new RecursiveDirectoryIterator("$work/resources", FilesystemIterator::SKIP_DOTS));
    foreach ($it as $f) {
        if (preg_match('/\.(jsx?|tsx?|css|vue)$/', $f->getFilename())) {
            $src = substr($f->getPathname(), strlen($work) + 1);
            $entries[$src] = ['file' => 'assets/' . md5($src) . (str_ends_with($src, '.css') ? '.css' : '.js'), 'src' => $src, 'isEntry' => true];
        }
    }
    // Also map asset paths referenced from views (fonts, images, node_modules files).
    $views = new RecursiveIteratorIterator(new RecursiveDirectoryIterator("$work/resources", FilesystemIterator::SKIP_DOTS));
    foreach ($views as $f) {
        if (!str_ends_with($f->getFilename(), '.php')) {
            continue;
        }
        preg_match_all('#[\'"]((?:resources|node_modules)/[^\'"\s]+\.(?:jsx?|tsx?|css|vue|woff2?|ttf|png|svg|jpe?g|webp|ico))[\'"]#', file_get_contents($f->getPathname()), $m);
        foreach ($m[1] as $src) {
            $entries[$src] ??= ['file' => 'assets/' . md5($src), 'src' => $src];
        }
    }
    @mkdir(dirname($manifest), 0777, true);
    file_put_contents($manifest, json_encode($entries));
}
passthru('composer dump-autoload --no-interaction --no-scripts -q --working-dir=' . escapeshellarg($work) . ' 2>&1', $rc);
if ($rc !== 0) { exit($rc); }
passthru('php ' . escapeshellarg("$work/vendor/bin/phpunit") . ' --colors=never 2>&1', $rc);
exit($rc);
