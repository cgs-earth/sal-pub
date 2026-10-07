FROM quay.io/keycloak/keycloak:26.7.0

COPY --chown=keycloak:root ./sal.json /opt/keycloak/sal.json
COPY --chmod=755 ./entrypoint.sh /opt/keycloak/entrypoint.sh

ENTRYPOINT [ "/opt/keycloak/entrypoint.sh" ]
CMD [ "start", "--import-realm", "--spi-theme--static-max-age=-1", "--spi-theme--cache-themes=false", "--spi-theme--cache-templates=false" ]
