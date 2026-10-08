FROM quay.io/keycloak/keycloak:26.7.0

# Fixed settings for this deployment. KC_HOSTNAME is set by Compose from
# SALPUB_HOSTNAME; credentials and identity providers come from .env.
# KC_HOSTNAME_BACKCHANNEL_DYNAMIC lets sal-pub use Keycloak's internal URL for
# token/JWKS calls while browsers keep using the public one.
ENV KC_DB=postgres \
    KC_DB_URL_HOST=postgres \
    KC_DB_URL_DATABASE=keycloak \
    KC_HTTP_ENABLED=true \
    KC_HTTP_RELATIVE_PATH=/auth \
    KC_PROXY_HEADERS=xforwarded \
    KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true

COPY --chown=keycloak:root ./sal.json /opt/keycloak/sal.json
COPY --chmod=755 ./entrypoint.sh /opt/keycloak/entrypoint.sh

ENTRYPOINT [ "/opt/keycloak/entrypoint.sh" ]
CMD [ "start", "--import-realm", "--spi-theme--static-max-age=-1", "--spi-theme--cache-themes=false", "--spi-theme--cache-templates=false" ]
