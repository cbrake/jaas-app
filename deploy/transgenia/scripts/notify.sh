#!/bin/sh
# Send a plain-text email through the landing's contact SMTP alias.
#
#   printf 'cuerpo' | notify.sh admin@transgenia.org "Asunto"
#
# PHPMailer runs inside the existing pagina-transgenia-php container, which
# already holds the SMTP settings (php.env). The PHP program and the body
# travel over the `docker exec -i` pipe: nothing is written to the landing
# docroot, and no SMTP value is read on the host or printed.
set -eu

to=${1:?recipient}
subject=${2:?subject}
container=${NOTIFY_CONTAINER:-pagina-transgenia-php}

body_b64=$(base64 -w0)
to_b64=$(printf '%s' "$to" | base64 -w0)
subject_b64=$(printf '%s' "$subject" | base64 -w0)

docker exec -i "$container" php <<PHP
<?php
declare(strict_types=1);
require '/var/www/html/vendor/autoload.php';
require '/var/www/html/src/autoload.php';
\Dotenv\Dotenv::createImmutable('/var/www/html')->safeLoad();
use Transgenia\Forms\Config\Env;
use PHPMailer\PHPMailer\PHPMailer;

\$m = new PHPMailer(true);
\$m->isSMTP();
\$m->Host = Env::string('SMTP_HOST', 'localhost');
\$m->Port = Env::int('SMTP_PORT', 587, 1, 65535);
\$m->CharSet = PHPMailer::CHARSET_UTF8;
\$secure = strtolower(Env::string('SMTP_SECURE', ''));
if (\$secure === 'tls') { \$m->SMTPSecure = PHPMailer::ENCRYPTION_STARTTLS; }
elseif (\$secure === 'ssl') { \$m->SMTPSecure = PHPMailer::ENCRYPTION_SMTPS; }
else { \$m->SMTPSecure = ''; \$m->SMTPAutoTLS = false; }
\$user = Env::string('SMTP_USER');
if (\$user !== '') { \$m->SMTPAuth = true; \$m->Username = \$user; \$m->Password = Env::string('SMTP_PASS'); }
\$m->setFrom(Env::string('MAIL_FROM_ADDRESS', 'noreply@transgenia.org'), 'Transgenia Meetings');
\$m->addAddress(base64_decode('$to_b64'));
\$m->Subject = base64_decode('$subject_b64');
\$m->Body = base64_decode('$body_b64');
try {
    \$m->send();
    fwrite(STDOUT, "sent\n");
} catch (\Throwable \$e) {
    fwrite(STDERR, "send failed: " . \$m->ErrorInfo . "\n");
    exit(1);
}
PHP
